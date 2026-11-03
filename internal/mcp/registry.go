package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sam/gemini-remote-linux/internal/config"
	"github.com/sam/gemini-remote-linux/internal/proto"
)

// BundledServer는 DesktopDeviceInfo.bundled_mcp_servers에 포함될 서버 선언이다.
type BundledServer struct {
	ServerID         string
	DisplayName      string
	Instruction      string
	Enabled          bool
	ToolDeclarations []proto.FunctionDecl
}

// Registry는 내장(git, obsidian) 및 외부 stdio MCP 서버를 관리하고 호출을 중계한다.
type Registry struct {
	mu      sync.RWMutex
	cfg     *config.Config
	servers map[string]*BundledServer
	reqSeq  atomic.Int64
}

func NewRegistry(cfg *config.Config) *Registry {
	r := &Registry{
		cfg:     cfg,
		servers: make(map[string]*BundledServer),
	}
	r.reload()
	return r
}

func (r *Registry) reload() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, sc := range r.cfg.MCPServers {
		if !sc.Enabled {
			continue
		}
		switch sc.ServerID {
		case "git":
			r.servers["git"] = &BundledServer{
				ServerID:         "git",
				DisplayName:       nonEmpty(sc.DisplayName, "Git"),
				Instruction:      nonEmpty(sc.Instruction, "Inspect and manage local Git repositories."),
				Enabled:          true,
				ToolDeclarations: gitToolDeclarations(),
			}
		case "obsidian":
			r.servers["obsidian"] = &BundledServer{
				ServerID:         "obsidian",
				DisplayName:      nonEmpty(sc.DisplayName, "Obsidian"),
				Instruction:      nonEmpty(sc.Instruction, "Read, search, and update local Markdown notes."),
				Enabled:          true,
				ToolDeclarations: obsidianToolDeclarations(),
			}
		default:
			decls := r.probeExternalMCPTools(sc)
			r.servers[sc.ServerID] = &BundledServer{
				ServerID:         sc.ServerID,
				DisplayName:      nonEmpty(sc.DisplayName, sc.ServerID),
				Instruction:      sc.Instruction,
				Enabled:          true,
				ToolDeclarations: decls,
			}
		}
	}
}

// BundledServers는 등록할 BundledMcpServer 목록을 반환한다.
func (r *Registry) BundledServers() []BundledServer {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make([]string, 0, len(r.servers))
	for k := range r.servers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]BundledServer, 0, len(keys))
	for _, k := range keys {
		out = append(out, *r.servers[k])
	}
	return out
}

// ListServersJSON은 list_mcp 도구 응답용 맵을 반환한다.
func (r *Registry) ListServersJSON() map[string]any {
	servers := r.BundledServers()
	items := make([]map[string]any, 0, len(servers))
	for _, s := range servers {
		tools := make([]string, 0, len(s.ToolDeclarations))
		for _, t := range s.ToolDeclarations {
			tools = append(tools, t.Name)
		}
		items = append(items, map[string]any{
			"server_id":    s.ServerID,
			"display_name": s.DisplayName,
			"instruction":  s.Instruction,
			"enabled":      s.Enabled,
			"tools":        tools,
		})
	}
	return map[string]any{"mcp_servers": items}
}

// Execute는 내장 또는 외부 MCP 도구를 실행한다.
func (r *Registry) Execute(ctx context.Context, serverID, toolName string, args map[string]any) (map[string]any, error) {
	if serverID == "" {
		// 도구 이름으로 서버 자동 추론
		if strings.HasPrefix(toolName, "git_") {
			serverID = "git"
		} else if strings.HasPrefix(toolName, "obsidian_") {
			serverID = "obsidian"
		} else if parts := strings.SplitN(toolName, "__", 2); len(parts) == 2 {
			serverID = parts[0]
			toolName = parts[1]
		}
	}

	switch serverID {
	case "git":
		return r.execGitTool(ctx, toolName, args)
	case "obsidian":
		return r.execObsidianTool(ctx, toolName, args)
	default:
		for _, sc := range r.cfg.MCPServers {
			if sc.ServerID == serverID && sc.Command != "" {
				return r.callExternalMCP(ctx, sc, toolName, args)
			}
		}
		return nil, fmt.Errorf("알 수 없는 MCP 서버 또는 도구: server=%q tool=%q", serverID, toolName)
	}
}

func (r *Registry) defaultRepoPath(args map[string]any) string {
	for _, k := range []string{"repo_path", "repository", "path", "working_directory"} {
		if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	if len(r.cfg.ConnectedFolders) > 0 {
		return r.cfg.ConnectedFolders[0]
	}
	wd, _ := os.Getwd()
	return wd
}

func (r *Registry) execGitTool(ctx context.Context, toolName string, args map[string]any) (map[string]any, error) {
	repo := r.defaultRepoPath(args)
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	runGit := func(gitArgs ...string) (map[string]any, error) {
		cmd := exec.CommandContext(cctx, "git", append([]string{"-C", repo}, gitArgs...)...)
		out, err := cmd.CombinedOutput()
		res := map[string]any{
			"repository": repo,
			"output":     string(out),
		}
		if err != nil {
			res["error"] = err.Error()
		}
		return res, nil
	}

	switch toolName {
	case "git_status":
		return runGit("status", "-sb")
	case "git_diff":
		staged, _ := args["staged"].(bool)
		if staged {
			return runGit("diff", "--staged")
		}
		if target, ok := args["target"].(string); ok && target != "" {
			return runGit("diff", target)
		}
		return runGit("diff")
	case "git_log":
		limit := "15"
		if n, ok := args["max_count"].(float64); ok && n > 0 {
			limit = fmt.Sprintf("%d", int(n))
		}
		return runGit("log", "-n", limit, "--oneline", "--decorate")
	case "git_show":
		rev := "HEAD"
		if v, ok := args["revision"].(string); ok && v != "" {
			rev = v
		}
		return runGit("show", rev)
	case "git_branch":
		return runGit("branch", "-a")
	case "git_checkout":
		branch, _ := args["branch"].(string)
		if branch == "" {
			return nil, fmt.Errorf("branch 파라미터가 필요합니다")
		}
		return runGit("checkout", branch)
	case "git_commit":
		msg, _ := args["message"].(string)
		if msg == "" {
			return nil, fmt.Errorf("message 파라미터가 필요합니다")
		}
		return runGit("commit", "-m", msg)
	default:
		return nil, fmt.Errorf("지원하지 않는 git MCP 도구: %s", toolName)
	}
}

func (r *Registry) vaultRoot(args map[string]any) string {
	if v, ok := args["vault_path"].(string); ok && v != "" {
		return v
	}
	if r.cfg.ObsidianVaultDir != "" {
		return r.cfg.ObsidianVaultDir
	}
	if len(r.cfg.ConnectedFolders) > 0 {
		return r.cfg.ConnectedFolders[0]
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Documents")
}

func (r *Registry) execObsidianTool(_ context.Context, toolName string, args map[string]any) (map[string]any, error) {
	vault := r.vaultRoot(args)
	switch toolName {
	case "obsidian_list_notes":
		var notes []string
		_ = filepath.WalkDir(vault, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
				rel, err := filepath.Rel(vault, path)
				if err == nil {
					notes = append(notes, rel)
				}
			}
			if len(notes) >= 200 {
				return io.EOF
			}
			return nil
		})
		return map[string]any{"vault": vault, "notes": notes}, nil

	case "obsidian_read_note":
		note, _ := args["note_path"].(string)
		full := filepath.Join(vault, note)
		data, err := os.ReadFile(full)
		if err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"note_path": note, "content": string(data)}, nil

	case "obsidian_write_note":
		note, _ := args["note_path"].(string)
		content, _ := args["content"].(string)
		full := filepath.Join(vault, note)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return map[string]any{"error": err.Error()}, nil
		}
		return map[string]any{"success": true, "note_path": note}, nil

	case "obsidian_search_notes":
		query, _ := args["query"].(string)
		qLower := strings.ToLower(query)
		var matches []map[string]any
		_ = filepath.WalkDir(vault, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
				data, err := os.ReadFile(path)
				if err == nil && strings.Contains(strings.ToLower(string(data)), qLower) {
					rel, _ := filepath.Rel(vault, path)
					matches = append(matches, map[string]any{"note_path": rel})
				}
			}
			if len(matches) >= 50 {
				return io.EOF
			}
			return nil
		})
		return map[string]any{"query": query, "matches": matches}, nil

	default:
		return nil, fmt.Errorf("지원하지 않는 obsidian MCP 도구: %s", toolName)
	}
}

func (r *Registry) probeExternalMCPTools(sc config.MCPServerConfig) []proto.FunctionDecl {
	if sc.Command == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := r.runStdioRPC(ctx, sc, "tools/list", map[string]any{})
	if err != nil {
		return nil
	}
	toolsAny, _ := resp["tools"].([]any)
	var out []proto.FunctionDecl
	for _, t := range toolsAny {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tm["name"].(string)
		desc, _ := tm["description"].(string)
		if name != "" {
			out = append(out, proto.FunctionDecl{
				Name:        name,
				Description: desc,
				Parameters:  &proto.SchemaDef{Type: proto.TypeObject},
			})
		}
	}
	return out
}

func (r *Registry) callExternalMCP(ctx context.Context, sc config.MCPServerConfig, toolName string, args map[string]any) (map[string]any, error) {
	return r.runStdioRPC(ctx, sc, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": args,
	})
}

func (r *Registry) runStdioRPC(ctx context.Context, sc config.MCPServerConfig, method string, params map[string]any) (map[string]any, error) {
	cmd := exec.CommandContext(ctx, sc.Command, sc.Args...)
	cmd.Env = os.Environ()
	for k, v := range sc.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	reader := bufio.NewReader(stdout)
	send := func(id int64, m string, p map[string]any) error {
		payload := map[string]any{
			"jsonrpc": "2.0",
			"method":  m,
			"params":  p,
		}
		if id > 0 {
			payload["id"] = id
		}
		b, _ := json.Marshal(payload)
		b = append(b, '\n')
		_, err := stdin.Write(b)
		return err
	}

	if err := send(1, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "gemini-remote-linux", "version": "1.0.0"},
	}); err != nil {
		return nil, err
	}
	_, _ = reader.ReadBytes('\n')
	_ = send(0, "notifications/initialized", map[string]any{})

	reqID := r.reqSeq.Add(1) + 10
	if err := send(reqID, method, params); err != nil {
		return nil, err
	}

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var msg struct {
			ID     int64          `json:"id"`
			Result map[string]any `json:"result"`
			Error  map[string]any `json:"error"`
		}
		if json.Unmarshal(line, &msg) == nil && msg.ID == reqID {
			if msg.Error != nil {
				return map[string]any{"error": msg.Error}, nil
			}
			return msg.Result, nil
		}
	}
}

func gitToolDeclarations() []proto.FunctionDecl {
	repoProp := map[string]*proto.SchemaDef{
		"repo_path": {Type: proto.TypeString, Description: "Absolute path to the Git repository"},
	}
	return []proto.FunctionDecl{
		{
			Name:        "git_status",
			Description: "Show the working tree status of a Git repository.",
			Parameters:  &proto.SchemaDef{Type: proto.TypeObject, Properties: repoProp},
		},
		{
			Name:        "git_diff",
			Description: "Show changes between commits or working tree.",
			Parameters: &proto.SchemaDef{
				Type: proto.TypeObject,
				Properties: map[string]*proto.SchemaDef{
					"repo_path": {Type: proto.TypeString, Description: "Repository path"},
					"staged":    {Type: proto.TypeBoolean, Description: "Show staged changes"},
					"target":    {Type: proto.TypeString, Description: "Optional revision or path"},
				},
			},
		},
		{
			Name:        "git_log",
			Description: "Show commit logs.",
			Parameters: &proto.SchemaDef{
				Type: proto.TypeObject,
				Properties: map[string]*proto.SchemaDef{
					"repo_path": {Type: proto.TypeString, Description: "Repository path"},
					"max_count": {Type: proto.TypeInteger, Description: "Maximum number of commits"},
				},
			},
		},
		{
			Name:        "git_show",
			Description: "Show commit details.",
			Parameters: &proto.SchemaDef{
				Type: proto.TypeObject,
				Properties: map[string]*proto.SchemaDef{
					"repo_path": {Type: proto.TypeString, Description: "Repository path"},
					"revision":  {Type: proto.TypeString, Description: "Commit revision"},
				},
			},
		},
		{
			Name:        "git_branch",
			Description: "List branches in the repository.",
			Parameters:  &proto.SchemaDef{Type: proto.TypeObject, Properties: repoProp},
		},
	}
}

func obsidianToolDeclarations() []proto.FunctionDecl {
	return []proto.FunctionDecl{
		{
			Name:        "obsidian_list_notes",
			Description: "List Markdown notes in the Obsidian vault.",
			Parameters: &proto.SchemaDef{
				Type: proto.TypeObject,
				Properties: map[string]*proto.SchemaDef{
					"vault_path": {Type: proto.TypeString, Description: "Optional vault root path"},
				},
			},
		},
		{
			Name:        "obsidian_read_note",
			Description: "Read a Markdown note from the Obsidian vault.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"note_path"},
				Properties: map[string]*proto.SchemaDef{
					"note_path":  {Type: proto.TypeString, Description: "Relative path to the note"},
					"vault_path": {Type: proto.TypeString, Description: "Optional vault root path"},
				},
			},
		},
		{
			Name:        "obsidian_write_note",
			Description: "Create or overwrite a Markdown note in the Obsidian vault.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"note_path", "content"},
				Properties: map[string]*proto.SchemaDef{
					"note_path":  {Type: proto.TypeString, Description: "Relative path to the note"},
					"content":    {Type: proto.TypeString, Description: "Markdown content"},
					"vault_path": {Type: proto.TypeString, Description: "Optional vault root path"},
				},
			},
		},
		{
			Name:        "obsidian_search_notes",
			Description: "Search Markdown notes in the Obsidian vault.",
			Parameters: &proto.SchemaDef{
				Type:     proto.TypeObject,
				Required: []string{"query"},
				Properties: map[string]*proto.SchemaDef{
					"query":      {Type: proto.TypeString, Description: "Search query"},
					"vault_path": {Type: proto.TypeString, Description: "Optional vault root path"},
				},
			},
		},
	}
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}
