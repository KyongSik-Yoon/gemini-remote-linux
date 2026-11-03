package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sam/gemini-remote-linux/internal/auth"
	"github.com/sam/gemini-remote-linux/internal/config"
	"github.com/sam/gemini-remote-linux/internal/cua"
	"github.com/sam/gemini-remote-linux/internal/mcp"
	"github.com/sam/gemini-remote-linux/internal/proto"
	"github.com/sam/gemini-remote-linux/internal/scotty"
)

// CallRequest는 RobinOp.ClientFunctionCallArgs.FunctionCall 하나에 대응한다.
type CallRequest struct {
	ID   string
	Name string
	Args map[string]any
}

// CallResponse는 ClientFunctionCallResult.FunctionResponse 하나에 대응한다.
type CallResponse struct {
	ID          string
	Name        string
	Response    map[string]any
	Attachments []scotty.BlobAttachment
	UIState     string
}

type bgProcess struct {
	mu       sync.Mutex
	id       string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	done     chan struct{}
	exitCode int
	exited   bool
}

// Executor는 macOS Gemini.app의 DesktopToolServiceImpl 전체 도구를 리눅스에서 실행한다.
type Executor struct {
	cfg              *config.Config
	authMgr          *auth.Manager
	uploader         *scotty.Uploader
	mcpReg           *mcp.Registry
	onFoldersChanged func()

	procMu sync.Mutex
	procs  map[string]*bgProcess
}

func NewExecutor(cfg *config.Config, authMgr *auth.Manager, uploader *scotty.Uploader, mcpReg *mcp.Registry) *Executor {
	return &Executor{
		cfg:      cfg,
		authMgr:  authMgr,
		uploader: uploader,
		mcpReg:   mcpReg,
		procs:    make(map[string]*bgProcess),
	}
}

func (e *Executor) SetOnFoldersChanged(fn func()) {
	e.onFoldersChanged = fn
}

// Declarations는 RegisterDeviceRequest.desktop_device_info.tool_declarations에 등록할 코어 도구 스키마를 반환한다.
func (e *Executor) Declarations() []proto.FunctionDecl {
	return []proto.FunctionDecl{
		{
			Name:        "run_command",
			Description: "Run any bash or shell command on the desktop machine, including system monitoring (top, free, ps, df, lscpu, /proc) and file/git commands.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"command"},
				Properties: map[string]*proto.SchemaDef{
					"command":           {Type: proto.TypeString, Description: "Shell command to execute"},
					"CommandLine":       {Type: proto.TypeString, Description: "Shell command to execute"},
					"working_directory": {Type: proto.TypeString, Description: "Working directory path"},
					"timeout_ms":        {Type: proto.TypeInteger, Description: "Timeout in milliseconds (default 30000)"},
				},
			},
		},
		{
			Name:        "command_status",
			Description: "Check the status and output of a background command.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"command_id"},
				Properties: map[string]*proto.SchemaDef{
					"command_id":   {Type: proto.TypeString, Description: "Command ID returned by run_command"},
					"wait_seconds": {Type: proto.TypeInteger, Description: "Seconds to wait before returning"},
				},
			},
		},
		{
			Name:        "send_command_input",
			Description: "Send stdin input or terminate a running background command.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"command_id"},
				Properties: map[string]*proto.SchemaDef{
					"command_id": {Type: proto.TypeString, Description: "Command ID"},
					"input":      {Type: proto.TypeString, Description: "Input text to write to stdin"},
					"terminate":  {Type: proto.TypeBoolean, Description: "Whether to terminate the command"},
					"wait_ms":    {Type: proto.TypeInteger, Description: "Milliseconds to wait after action"},
				},
			},
		},
		{
			Name:        "run_python",
			Description: "Execute Python 3 code locally.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"code"},
				Properties: map[string]*proto.SchemaDef{
					"code":              {Type: proto.TypeString, Description: "Python source code to execute"},
					"working_directory": {Type: proto.TypeString, Description: "Optional working directory"},
				},
			},
		},
		{
			Name:        "read_file",
			Description: "Read text contents of a local file.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"path"},
				Properties: map[string]*proto.SchemaDef{
					"path":       {Type: proto.TypeString, Description: "Absolute path to the file"},
					"start_line": {Type: proto.TypeInteger, Description: "1-based start line"},
					"end_line":   {Type: proto.TypeInteger, Description: "1-based end line"},
					"limit":      {Type: proto.TypeInteger, Description: "Max lines to read"},
				},
			},
		},
		{
			Name:        "view_file",
			Description: "View an image, PDF, or binary file by uploading it as an attachment.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"path"},
				Properties: map[string]*proto.SchemaDef{
					"path": {Type: proto.TypeString, Description: "Absolute path to the file"},
				},
			},
		},
		{
			Name:        "write_file",
			Description: "Write or overwrite a file on the local filesystem.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"path", "content"},
				Properties: map[string]*proto.SchemaDef{
					"path":    {Type: proto.TypeString, Description: "Absolute path to the file"},
					"content": {Type: proto.TypeString, Description: "Full text content to write"},
				},
			},
		},
		{
			Name:        "replace_file_content",
			Description: "Replace an exact substring in a file.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"path", "old_str", "new_str"},
				Properties: map[string]*proto.SchemaDef{
					"path":    {Type: proto.TypeString, Description: "Absolute path to the file"},
					"old_str": {Type: proto.TypeString, Description: "Existing text to replace"},
					"new_str": {Type: proto.TypeString, Description: "Replacement text"},
				},
			},
		},
		{
			Name:        "multi_replace_file_content",
			Description: "Apply multiple substring replacements to a file.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"path", "replacement_chunks"},
				Properties: map[string]*proto.SchemaDef{
					"path": {Type: proto.TypeString, Description: "Absolute path to the file"},
					"replacement_chunks": {
						Type: proto.TypeArray,
						Items: &proto.SchemaDef{
							Type:     proto.TypeObject,
							Required: []string{"old_str", "new_str"},
							Properties: map[string]*proto.SchemaDef{
								"old_str": {Type: proto.TypeString},
								"new_str": {Type: proto.TypeString},
							},
						},
					},
				},
			},
		},
		{
			Name:        "list_directory",
			Description: "List files and subdirectories in a directory.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"path"},
				Properties: map[string]*proto.SchemaDef{
					"path":           {Type: proto.TypeString, Description: "Directory path"},
					"DirectoryPath":  {Type: proto.TypeString, Description: "Directory path"},
					"recursive":      {Type: proto.TypeBoolean, Description: "List recursively"},
					"max_depth":      {Type: proto.TypeInteger, Description: "Maximum recursion depth"},
				},
			},
		},
		{
			Name:        "list_dir",
			Description: "List files and subdirectories in a directory.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"DirectoryPath"},
				Properties: map[string]*proto.SchemaDef{
					"DirectoryPath": {Type: proto.TypeString, Description: "Absolute path to directory"},
					"path":          {Type: proto.TypeString, Description: "Directory path"},
				},
			},
		},
		{
			Name:        "find_by_name",
			Description: "Find files or directories matching a name pattern.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"pattern", "directory"},
				Properties: map[string]*proto.SchemaDef{
					"pattern":   {Type: proto.TypeString, Description: "Substring or glob pattern"},
					"directory": {Type: proto.TypeString, Description: "Base directory to search"},
					"max_depth": {Type: proto.TypeInteger, Description: "Max search depth"},
					"type":      {Type: proto.TypeString, Description: "file, directory, or any"},
				},
			},
		},
		{
			Name:        "grep_search",
			Description: "Search file contents for a text or regex query.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"query", "path"},
				Properties: map[string]*proto.SchemaDef{
					"query":            {Type: proto.TypeString, Description: "Search pattern"},
					"path":             {Type: proto.TypeString, Description: "File or directory path"},
					"is_regex":         {Type: proto.TypeBoolean, Description: "Whether query is a regex"},
					"case_insensitive": {Type: proto.TypeBoolean, Description: "Case-insensitive search"},
				},
			},
		},
		{
			Name:        "find_folders",
			Description: "Find folders on the computer matching a query.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"query"},
				Properties: map[string]*proto.SchemaDef{
					"query":     {Type: proto.TypeString, Description: "Folder name query"},
					"max_depth": {Type: proto.TypeInteger, Description: "Max search depth"},
				},
			},
		},
		{
			Name:        "connect_folders",
			Description: "Connect local folders so Gemini can access them.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"paths"},
				Properties: map[string]*proto.SchemaDef{
					"paths": {
						Type:  proto.TypeArray,
						Items: &proto.SchemaDef{Type: proto.TypeString},
					},
				},
			},
		},
		{
			Name:        "list_skills",
			Description: "List installed local skills.",
			Parameters:  &proto.SchemaDef{Type: proto.TypeObject},
		},
		{
			Name:        "ReadSkill",
			Description: "Read a skill's SKILL.md instructions by skill name.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"name"},
				Properties: map[string]*proto.SchemaDef{
					"name": {Type: proto.TypeString, Description: "Skill name"},
				},
			},
		},
		{
			Name:        "list_mcp",
			Description: "List configured MCP servers and their tools.",
			Parameters:  &proto.SchemaDef{Type: proto.TypeObject},
		},
		{
			Name:        "execute_mcp",
			Description: "Execute a tool on an MCP server.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"ToolName"},
				Properties: map[string]*proto.SchemaDef{
					"ServerName": {Type: proto.TypeString, Description: "MCP server ID"},
					"ToolName":   {Type: proto.TypeString, Description: "Tool name"},
					"Arguments":  {Type: proto.TypeObject, Description: "Tool arguments"},
				},
			},
		},
		{
			Name:        "ui_control",
			Description: "Execute Python GUI automation script (click, move_to, type_text, press_key, hotkey, scroll, open_app, screenshot, observe_window).",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"code"},
				Properties: map[string]*proto.SchemaDef{
					"code": {Type: proto.TypeString, Description: "Python UI automation code"},
				},
			},
		},
		{
			Name:        "upload_to_drive",
			Description: "Upload a local file (such as a screenshot, image, PDF, or document) to the user's Google Drive and return a web_view_link URL so the user can view or open it directly in the chat.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"FilePath"},
				Properties: map[string]*proto.SchemaDef{
					"FilePath":                  {Type: proto.TypeString, Description: "Absolute path to the local file to upload"},
					"path":                      {Type: proto.TypeString, Description: "Absolute path to the local file to upload"},
					"FolderId":                  {Type: proto.TypeString, Description: "Optional Google Drive destination folder ID"},
					"FileName":                  {Type: proto.TypeString, Description: "Optional file name in Google Drive"},
					"ConvertToWorkspaceFormat":  {Type: proto.TypeBoolean, Description: "Whether to convert to Google Docs/Sheets/Slides format"},
					"IgnoreSharedFolderWarning": {Type: proto.TypeBoolean, Description: "Whether to ignore shared folder warnings"},
				},
			},
		},
		{
			Name:        "download_from_drive",
			Description: "Download a file from Google Drive to the local filesystem.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"FileId", "AbsolutePath"},
				Properties: map[string]*proto.SchemaDef{
					"FileId":              {Type: proto.TypeString, Description: "Google Drive file ID"},
					"AbsolutePath":        {Type: proto.TypeString, Description: "Destination local file path"},
					"IgnoreBackupFailure": {Type: proto.TypeBoolean, Description: "Ignore backup failure"},
				},
			},
		},
		{
			Name:        "export_from_drive",
			Description: "Export a Google Workspace document from Google Drive to a local file.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"FileId", "AbsolutePath", "MimeType"},
				Properties: map[string]*proto.SchemaDef{
					"FileId":       {Type: proto.TypeString, Description: "Google Drive file ID"},
					"AbsolutePath": {Type: proto.TypeString, Description: "Destination local file path"},
					"MimeType":     {Type: proto.TypeString, Description: "Target export MIME type (e.g. application/pdf)"},
				},
			},
		},
	}
}

// ExecuteCall은 단일 함수 호출을 처리하고 CallResponse를 반환한다.
func (e *Executor) ExecuteCall(ctx context.Context, call CallRequest, skipPostScreenshot bool) CallResponse {
	// "client:" 접두사 제거 (DesktopToolServiceImpl 동작과 동일)
	cleanName := strings.TrimPrefix(call.Name, "client:")
	res := CallResponse{
		ID:   call.ID,
		Name: call.Name, // 응답에는 원래 호출된 이름 유지
	}

	out, attachments, uiState, err := e.dispatch(ctx, cleanName, call.Args, skipPostScreenshot)
	if err != nil {
		res.Response = map[string]any{"error": err.Error()}
		return res
	}
	res.Response = out
	res.Attachments = attachments
	res.UIState = uiState
	return res
}

func (e *Executor) dispatch(ctx context.Context, name string, args map[string]any, skipPostScreenshot bool) (map[string]any, []scotty.BlobAttachment, string, error) {
	switch name {
	case "run_command":
		r, err := e.toolRunCommand(ctx, args)
		return r, nil, "", err
	case "command_status":
		r, err := e.toolCommandStatus(ctx, args)
		return r, nil, "", err
	case "send_command_input":
		r, err := e.toolSendCommandInput(ctx, args)
		return r, nil, "", err
	case "run_python":
		r, err := e.toolRunPython(ctx, args)
		return r, nil, "", err
	case "read_file", "view_file_outline", "view_code_item":
		r, err := e.toolReadFile(args)
		return r, nil, "", err
	case "view_file":
		return e.toolViewFile(ctx, args)
	case "write_file", "write_to_file", "create_file":
		r, err := e.toolWriteFile(args)
		return r, nil, "", err
	case "replace_file_content":
		r, err := e.toolReplaceFileContent(args)
		return r, nil, "", err
	case "multi_replace_file_content":
		r, err := e.toolMultiReplaceFileContent(args)
		return r, nil, "", err
	case "list_directory", "list_dir":
		r, err := e.toolListDirectory(args)
		return r, nil, "", err
	case "find_by_name":
		r, err := e.toolFindByName(args)
		return r, nil, "", err
	case "grep_search":
		r, err := e.toolGrepSearch(args)
		return r, nil, "", err
	case "find_folders":
		r, err := e.toolFindFolders(args)
		return r, nil, "", err
	case "connect_folders":
		r, err := e.toolConnectFolders(args)
		return r, nil, "", err
	case "list_skills":
		r, err := e.toolListSkills()
		return r, nil, "", err
	case "ReadSkill", "lookup_skill", "read_skill":
		r, err := e.toolReadSkill(args)
		return r, nil, "", err
	case "list_mcp":
		return e.mcpReg.ListServersJSON(), nil, "", nil
	case "execute_mcp":
		srv := strArg(args, "ServerName", "server_name", "server")
		tool := strArg(args, "ToolName", "tool_name", "tool")
		subArgs, _ := args["Arguments"].(map[string]any)
		if subArgs == nil {
			subArgs, _ = args["arguments"].(map[string]any)
		}
		r, err := e.mcpReg.Execute(ctx, srv, tool, subArgs)
		return r, nil, "", err
	case "ui_control":
		return e.toolUIControl(ctx, args, skipPostScreenshot)
	case "upload_to_drive":
		r, err := e.toolUploadToDrive(ctx, args)
		return r, nil, "", err
	case "download_from_drive":
		r, err := e.toolDownloadFromDrive(ctx, args)
		return r, nil, "", err
	case "export_from_drive":
		r, err := e.toolExportFromDrive(ctx, args)
		return r, nil, "", err
	default:
		// MCP 도구 직접 호출 라우팅 (git_*, obsidian_*, <server>__<tool>)
		r, err := e.mcpReg.Execute(ctx, "", name, args)
		return r, nil, "", err
	}
}

func (e *Executor) defaultWorkDir(args map[string]any) string {
	wd := strArg(args, "working_directory", "cwd", "directory")
	if wd != "" {
		return expandHome(wd)
	}
	if len(e.cfg.ConnectedFolders) > 0 {
		return e.cfg.ConnectedFolders[0]
	}
	d, _ := os.Getwd()
	return d
}

func (e *Executor) toolRunCommand(_ context.Context, args map[string]any) (map[string]any, error) {
	cmdStr := strArg(args, "command", "CommandLine")
	if cmdStr == "" {
		return nil, fmt.Errorf("command 파라미터가 비어 있습니다")
	}
	wd := e.defaultWorkDir(args)
	timeoutMs := intArg(args, 30000, "timeout_ms", "WaitMsBeforeAsync")
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	if timeoutMs > 300000 {
		timeoutMs = 300000
	}

	cmd := exec.Command("bash", "-c", cmdStr)
	cmd.Dir = wd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	proc := &bgProcess{
		id:    newShortID(),
		cmd:   cmd,
		stdin: stdin,
		done:  make(chan struct{}),
	}
	cmd.Stdout = &proc.stdout
	cmd.Stderr = &proc.stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	e.procMu.Lock()
	e.procs[proc.id] = proc
	e.procMu.Unlock()

	go func() {
		err := cmd.Wait()
		proc.mu.Lock()
		proc.exited = true
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				proc.exitCode = ee.ExitCode()
			} else {
				proc.exitCode = 1
			}
		}
		proc.mu.Unlock()
		close(proc.done)
	}()

	select {
	case <-proc.done:
		proc.mu.Lock()
		defer proc.mu.Unlock()
		return map[string]any{
			"status":    "completed",
			"exit_code": proc.exitCode,
			"stdout":    truncateStr(proc.stdout.String(), 64000),
			"stderr":    truncateStr(proc.stderr.String(), 64000),
		}, nil
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		proc.mu.Lock()
		defer proc.mu.Unlock()
		return map[string]any{
			"command_id": proc.id,
			"status":     "running",
			"stdout":     truncateStr(proc.stdout.String(), 64000),
			"stderr":     truncateStr(proc.stderr.String(), 64000),
		}, nil
	}
}

func (e *Executor) toolCommandStatus(_ context.Context, args map[string]any) (map[string]any, error) {
	id := strArg(args, "command_id", "CommandId")
	waitSec := intArg(args, 5, "wait_seconds", "WaitDurationSeconds")
	if waitSec < 0 {
		waitSec = 0
	}
	if waitSec > 60 {
		waitSec = 60
	}

	e.procMu.Lock()
	proc := e.procs[id]
	e.procMu.Unlock()
	if proc == nil {
		return nil, fmt.Errorf("실행 중인 command_id=%q를 찾을 수 없습니다", id)
	}

	if waitSec > 0 {
		select {
		case <-proc.done:
		case <-time.After(time.Duration(waitSec) * time.Second):
		}
	}

	proc.mu.Lock()
	defer proc.mu.Unlock()
	status := "running"
	if proc.exited {
		status = "completed"
	}
	return map[string]any{
		"command_id": proc.id,
		"status":     status,
		"exit_code":  proc.exitCode,
		"stdout":     truncateStr(proc.stdout.String(), 64000),
		"stderr":     truncateStr(proc.stderr.String(), 64000),
	}, nil
}

func (e *Executor) toolSendCommandInput(ctx context.Context, args map[string]any) (map[string]any, error) {
	id := strArg(args, "command_id", "CommandId")
	input := strArg(args, "input", "Input")
	terminate := boolArg(args, "terminate", "Terminate")
	waitMs := intArg(args, 500, "wait_ms", "WaitMs")

	e.procMu.Lock()
	proc := e.procs[id]
	e.procMu.Unlock()
	if proc == nil {
		return nil, fmt.Errorf("command_id=%q를 찾을 수 없습니다", id)
	}

	if terminate && proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
	} else if input != "" && proc.stdin != nil {
		_, _ = io.WriteString(proc.stdin, input)
	}

	if waitMs > 0 {
		select {
		case <-proc.done:
		case <-time.After(time.Duration(waitMs) * time.Millisecond):
		}
	}
	return e.toolCommandStatus(ctx, map[string]any{"command_id": id, "wait_seconds": 0})
}

func (e *Executor) toolRunPython(ctx context.Context, args map[string]any) (map[string]any, error) {
	code := strArg(args, "code", "Code")
	wd := e.defaultWorkDir(args)
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, "python3", "-c", code)
	cmd.Dir = wd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = 1
		}
	}
	return map[string]any{
		"stdout":    truncateStr(stdout.String(), 64000),
		"stderr":    truncateStr(stderr.String(), 64000),
		"exit_code": exitCode,
	}, nil
}

func (e *Executor) toolReadFile(args map[string]any) (map[string]any, error) {
	p := expandHome(strArg(args, "path", "AbsolutePath"))
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	totalLines := len(lines)

	startLine := intArg(args, 1, "start_line", "StartLine")
	if startLine < 1 {
		startLine = 1
	}
	endLine := intArg(args, totalLines, "end_line", "EndLine")
	limit := intArg(args, 800, "limit")
	if endLine <= 0 || endLine > totalLines {
		endLine = totalLines
	}
	if startLine > totalLines {
		return map[string]any{
			"content":     "",
			"total_lines": totalLines,
			"start_line":  startLine,
			"end_line":    totalLines,
			"truncated":   false,
		}, nil
	}
	truncated := false
	if endLine-startLine+1 > limit {
		endLine = startLine + limit - 1
		truncated = true
	}
	slice := strings.Join(lines[startLine-1:endLine], "\n")
	return map[string]any{
		"content":     slice,
		"total_lines": totalLines,
		"start_line":  startLine,
		"end_line":    endLine,
		"truncated":   truncated,
	}, nil
}

func (e *Executor) toolViewFile(ctx context.Context, args map[string]any) (map[string]any, []scotty.BlobAttachment, string, error) {
	p := expandHome(strArg(args, "path", "AbsolutePath"))
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, nil, "", err
	}
	token, err := e.authMgr.AccessToken(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	mimeType := scotty.DetectMimeType(p, data)
	fileName := filepath.Base(p)
	blob, err := e.uploader.Upload(ctx, token, fileName, mimeType, data, scotty.FileSourceToolGenerated)
	if err != nil {
		return nil, nil, "", err
	}
	res := map[string]any{
		"status":    "attached",
		"path":      p,
		"mime_type": mimeType,
		"size":      len(data),
	}
	if driveRes, driveErr := e.toolUploadToDrive(ctx, map[string]any{"FilePath": p}); driveErr == nil {
		if link, ok := driveRes["web_view_link"].(string); ok && link != "" {
			res["web_view_link"] = link
			res["drive_file_id"] = driveRes["file_id"]
			res["assistant_instruction"] = fmt.Sprintf(
				"IMPORTANT: The user on mobile cannot see local files inline. You MUST include this clickable Markdown link in your response so the user can view the file directly: [%s (Google Drive에서 보기)](%s)",
				fileName, link,
			)
		}
	}
	return res, []scotty.BlobAttachment{*blob}, "", nil
}

func (e *Executor) toolWriteFile(args map[string]any) (map[string]any, error) {
	p := expandHome(strArg(args, "path", "TargetFile", "AbsolutePath"))
	content := strArg(args, "content", "Content")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return map[string]any{
		"success":       true,
		"path":          p,
		"bytes_written": len(content),
	}, nil
}

func (e *Executor) toolReplaceFileContent(args map[string]any) (map[string]any, error) {
	p := expandHome(strArg(args, "path", "TargetFile"))
	oldStr := strArg(args, "old_str", "TargetContent")
	newStr := strArg(args, "new_str", "ReplacementContent")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	text := string(data)
	if !strings.Contains(text, oldStr) {
		return nil, fmt.Errorf("파일에서 old_str를 찾을 수 없습니다: %s", p)
	}
	updated := strings.Replace(text, oldStr, newStr, 1)
	if err := os.WriteFile(p, []byte(updated), 0o644); err != nil {
		return nil, err
	}
	return map[string]any{"success": true, "path": p}, nil
}

func (e *Executor) toolMultiReplaceFileContent(args map[string]any) (map[string]any, error) {
	p := expandHome(strArg(args, "path", "TargetFile"))
	chunks, _ := args["replacement_chunks"].([]any)
	if len(chunks) == 0 {
		chunks, _ = args["ReplacementChunks"].([]any)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	text := string(data)
	applied := 0
	for _, c := range chunks {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		oldStr := strArg(cm, "old_str", "TargetContent")
		newStr := strArg(cm, "new_str", "ReplacementContent")
		if oldStr != "" && strings.Contains(text, oldStr) {
			text = strings.Replace(text, oldStr, newStr, 1)
			applied++
		}
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		return nil, err
	}
	return map[string]any{"success": true, "path": p, "applied_edits": applied}, nil
}

func (e *Executor) toolListDirectory(args map[string]any) (map[string]any, error) {
	p := expandHome(strArg(args, "path", "DirectoryPath"))
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, ent := range entries {
		info, _ := ent.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		out = append(out, map[string]any{
			"name":         ent.Name(),
			"path":         filepath.Join(p, ent.Name()),
			"is_directory": ent.IsDir(),
			"size":         size,
		})
	}
	return map[string]any{"entries": out}, nil
}

func (e *Executor) toolFindByName(args map[string]any) (map[string]any, error) {
	pattern := strings.ToLower(strings.TrimSpace(strArg(args, "pattern", "Pattern")))
	dir := expandHome(strArg(args, "directory", "SearchDirectory", "path"))
	maxDepth := intArg(args, 6, "max_depth", "MaxDepth")
	matches := []string{}
	truncated := false

	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		depth := strings.Count(rel, string(os.PathSeparator)) + 1
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || depth > maxDepth) {
			return filepath.SkipDir
		}
		nameLower := strings.ToLower(d.Name())
		matched := false
		if pattern == "" || pattern == "*" {
			matched = true
		} else if strings.ContainsAny(pattern, "*?[") {
			if ok, _ := filepath.Match(pattern, nameLower); ok {
				matched = true
			}
		} else if strings.Contains(nameLower, pattern) {
			matched = true
		}
		if matched {
			matches = append(matches, path)
			if len(matches) >= 200 {
				truncated = true
				return io.EOF
			}
		}
		return nil
	})
	return map[string]any{"matches": matches, "truncated": truncated}, nil
}

func (e *Executor) toolGrepSearch(args map[string]any) (map[string]any, error) {
	query := strArg(args, "query", "Query")
	searchPath := expandHome(strArg(args, "path", "SearchPath"))
	caseInsensitive := boolArg(args, "case_insensitive")
	flags := ""
	if caseInsensitive {
		flags = "(?i)"
	}
	re, err := regexp.Compile(flags + query)
	if err != nil {
		re = regexp.MustCompile(regexp.QuoteMeta(query))
	}

	var matches []map[string]any
	truncated := false

	_ = filepath.WalkDir(searchPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 2*1024*1024 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(data, 0) != -1 {
			return nil
		}
		for idx, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				matches = append(matches, map[string]any{
					"path":         path,
					"line_number":  idx + 1,
					"line_content": truncateStr(line, 300),
				})
				if len(matches) >= 100 {
					truncated = true
					return io.EOF
				}
			}
		}
		return nil
	})
	return map[string]any{"matches": matches, "truncated": truncated}, nil
}

func (e *Executor) toolFindFolders(args map[string]any) (map[string]any, error) {
	query := strings.ToLower(strArg(args, "query", "FolderPattern"))
	maxDepth := intArg(args, 3, "max_depth")
	home, _ := os.UserHomeDir()
	var folders []string

	_ = filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(home, path)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		depth := strings.Count(rel, string(os.PathSeparator)) + 1
		if strings.Contains(strings.ToLower(d.Name()), query) {
			folders = append(folders, path)
			if len(folders) >= 50 {
				return io.EOF
			}
		}
		if depth >= maxDepth {
			return filepath.SkipDir
		}
		return nil
	})
	return map[string]any{"folders": folders}, nil
}

func (e *Executor) toolConnectFolders(args map[string]any) (map[string]any, error) {
	var paths []string
	if raw, ok := args["paths"].([]any); ok {
		for _, item := range raw {
			if s, ok := item.(string); ok {
				paths = append(paths, expandHome(s))
			}
		}
	}
	var connected, rejected []string
	existing := make(map[string]bool)
	for _, f := range e.cfg.ConnectedFolders {
		existing[f] = true
	}
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			connected = append(connected, p)
			if !existing[p] {
				e.cfg.ConnectedFolders = append(e.cfg.ConnectedFolders, p)
				existing[p] = true
			}
		} else {
			rejected = append(rejected, p)
		}
	}
	_ = e.cfg.Save()
	if len(connected) > 0 && e.onFoldersChanged != nil {
		go e.onFoldersChanged()
	}
	return map[string]any{
		"connected_folders": connected,
		"rejected_folders":  rejected,
	}, nil
}

func (e *Executor) skillDirs() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".gemini", "config", "skills"),
		filepath.Join(home, ".gemini", "antigravity-cli", "builtin", "skills"),
	}
}

func (e *Executor) toolListSkills() (map[string]any, error) {
	var skills []map[string]any
	for _, base := range e.skillDirs() {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			skillMD := filepath.Join(base, ent.Name(), "SKILL.md")
			if _, err := os.Stat(skillMD); err == nil {
				skills = append(skills, map[string]any{
					"name": ent.Name(),
					"path": skillMD,
				})
			}
		}
	}
	return map[string]any{"skills": skills}, nil
}

func (e *Executor) toolReadSkill(args map[string]any) (map[string]any, error) {
	name := strArg(args, "name", "skill_name")
	for _, base := range e.skillDirs() {
		skillMD := filepath.Join(base, name, "SKILL.md")
		if data, err := os.ReadFile(skillMD); err == nil {
			return map[string]any{
				"name":    name,
				"content": string(data),
				"path":    skillMD,
			}, nil
		}
	}
	return nil, fmt.Errorf("스킬 %q를 찾을 수 없습니다", name)
}

func (e *Executor) toolUIControl(ctx context.Context, args map[string]any, skipPostScreenshot bool) (map[string]any, []scotty.BlobAttachment, string, error) {
	code := strArg(args, "code", "Code")
	res, err := cua.RunUIControl(ctx, code, !skipPostScreenshot)
	if err != nil {
		return nil, nil, "", err
	}
	var attachments []scotty.BlobAttachment
	if len(res.ScreenshotJPEG) > 0 {
		if token, err := e.authMgr.AccessToken(ctx); err == nil {
			if blob, err := e.uploader.Upload(ctx, token, "cua_screenshot.jpg", "image/jpeg", res.ScreenshotJPEG, scotty.FileSourceRobinAutomation); err == nil {
				attachments = append(attachments, *blob)
			}
		}
	}
	return map[string]any{
		"stdout":    res.Stdout,
		"stderr":    res.Stderr,
		"exit_code": res.ExitCode,
	}, attachments, res.UIState, nil
}

func (e *Executor) toolUploadToDrive(ctx context.Context, args map[string]any) (map[string]any, error) {
	filePath := expandHome(strArg(args, "FilePath", "path", "file_path", "AbsolutePath"))
	if filePath == "" {
		return nil, fmt.Errorf("Missing required parameter: FilePath")
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("Cannot upload empty (0-byte) file '%s' to Google Drive.", filePath)
	}

	token, err := e.authMgr.AccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("No access token available for Drive operations: %w", err)
	}

	fileName := strArg(args, "FileName", "file_name", "name")
	if fileName == "" {
		fileName = filepath.Base(filePath)
	}
	folderID := strArg(args, "FolderId", "folder_id", "parent_folder_id")
	mimeType := scotty.DetectMimeType(filePath, data)

	meta := map[string]any{
		"name": fileName,
	}
	if folderID != "" {
		meta["parents"] = []string{folderID}
	}
	if boolArg(args, "ConvertToWorkspaceFormat", "convert_to_workspace_format") {
		switch mimeType {
		case "text/csv":
			meta["mimeType"] = "application/vnd.google-apps.spreadsheet"
		case "text/plain", "text/markdown":
			meta["mimeType"] = "application/vnd.google-apps.document"
		}
	}
	metaJSON, _ := json.Marshal(meta)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	metaHdr := make(textproto.MIMEHeader)
	metaHdr.Set("Content-Type", "application/json; charset=UTF-8")
	metaPart, err := mw.CreatePart(metaHdr)
	if err != nil {
		return nil, err
	}
	_, _ = metaPart.Write(metaJSON)

	mediaHdr := make(textproto.MIMEHeader)
	mediaHdr.Set("Content-Type", mimeType)
	mediaPart, err := mw.CreatePart(mediaHdr)
	if err != nil {
		return nil, err
	}
	_, _ = mediaPart.Write(data)
	_ = mw.Close()

	uploadURL := "https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart&fields=id,name,mimeType,webViewLink,webContentLink"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Failed to upload file to Drive: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Failed to upload file to Drive (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var parsed struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		MimeType       string `json:"mimeType"`
		WebViewLink    string `json:"webViewLink"`
		WebContentLink string `json:"webContentLink"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}
	if parsed.WebViewLink == "" && parsed.ID != "" {
		parsed.WebViewLink = fmt.Sprintf("https://drive.google.com/file/d/%s/view", parsed.ID)
	}
	return map[string]any{
		"success":         true,
		"drive_file_id":   parsed.ID,
		"file_id":         parsed.ID,
		"name":            parsed.Name,
		"mime_type":       parsed.MimeType,
		"web_view_link":   parsed.WebViewLink,
		"drive_url":       parsed.WebViewLink,
		"web_content_url": parsed.WebContentLink,
	}, nil
}

func (e *Executor) toolDownloadFromDrive(ctx context.Context, args map[string]any) (map[string]any, error) {
	fileID := strArg(args, "FileId", "file_id", "drive_file_id")
	destPath := expandHome(strArg(args, "AbsolutePath", "path", "TargetFile", "destination_path"))
	if fileID == "" {
		return nil, fmt.Errorf("Missing required parameter: FileId")
	}
	if destPath == "" {
		return nil, fmt.Errorf("Missing required parameter: AbsolutePath")
	}
	token, err := e.authMgr.AccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("No access token available for Drive operations: %w", err)
	}

	dlURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?alt=media", url.PathEscape(fileID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Failed to download file from Drive: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Failed to download file from Drive (HTTP %d): %s", resp.StatusCode, string(data))
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		return nil, err
	}
	return map[string]any{
		"success":       true,
		"path":          destPath,
		"bytes_written": len(data),
	}, nil
}

func (e *Executor) toolExportFromDrive(ctx context.Context, args map[string]any) (map[string]any, error) {
	fileID := strArg(args, "FileId", "file_id", "drive_file_id")
	destPath := expandHome(strArg(args, "AbsolutePath", "path", "TargetFile", "destination_path"))
	mimeType := strArg(args, "MimeType", "mime_type")
	if fileID == "" {
		return nil, fmt.Errorf("Missing required parameter: FileId")
	}
	if destPath == "" {
		return nil, fmt.Errorf("Missing required parameter: AbsolutePath")
	}
	if mimeType == "" {
		mimeType = "application/pdf"
	}
	token, err := e.authMgr.AccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("No access token available for Drive operations: %w", err)
	}

	expURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s/export?mimeType=%s",
		url.PathEscape(fileID), url.QueryEscape(mimeType))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, expURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Failed to export file from Drive: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Failed to export file from Drive (HTTP %d): %s", resp.StatusCode, string(data))
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		return nil, err
	}
	return map[string]any{
		"success":       true,
		"path":          destPath,
		"mime_type":     mimeType,
		"bytes_written": len(data),
	}, nil
}

func strArg(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func intArg(args map[string]any, def int, keys ...string) int {
	for _, k := range keys {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		}
	}
	return def
}

func boolArg(args map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := args[k].(bool); ok {
			return v
		}
	}
	return false
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func truncateStr(s string, maxLen int) string {
	// 단일 라인이 비정상적으로 긴 base64/바이너리 덤프인 경우 서버 대화방 컨텍스트 오염 방지
	for _, line := range strings.Split(s, "\n") {
		if len(line) > 4096 && !strings.ContainsAny(line[:4096], " \t") {
			return "[binary/base64 output omitted to prevent conversation context corruption — use view_file or upload_to_drive to view or share files]"
		}
	}
	if maxLen > 16000 {
		maxLen = 16000
	}
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n...[truncated]"
}

func newShortID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("cmd_%x", b[:])
}
