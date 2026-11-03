package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sam/gemini-remote-linux/internal/auth"
	"github.com/sam/gemini-remote-linux/internal/config"
	"github.com/sam/gemini-remote-linux/internal/daemon"
	"github.com/sam/gemini-remote-linux/internal/mcp"
	"github.com/sam/gemini-remote-linux/internal/scotty"
	"github.com/sam/gemini-remote-linux/internal/tools"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadOrInit()
	if err != nil {
		fatalf("설정 로드 실패: %v", err)
	}
	authMgr := auth.NewManager()
	_ = authMgr.LoadFromDisk()

	cmd := os.Args[1]
	switch cmd {
	case "extract-keys":
		if len(os.Args) < 3 {
			fatalf("사용법: gemini-remote extract-keys <Gemini.app 또는 Gemini 바이너리 경로>")
		}
		cid, csec, err := auth.ExtractCredentialsFromApp(os.Args[2])
		if err != nil {
			fatalf("OAuth 자격증명 추출 실패: %v", err)
		}
		cfg.OAuthClientID = cid
		cfg.OAuthClientSecret = csec
		if err := cfg.Save(); err != nil {
			fatalf("설정 파일 저장 실패: %v", err)
		}
		cfgPath, _ := config.ConfigPath()
		fmt.Printf("OAuth 자격증명 추출 및 저장 완료 (%s)\n", cfgPath)

	case "login":
		noBrowser := len(os.Args) > 2 && os.Args[2] == "--no-browser"
		if err := authMgr.LoginInteractive(ctx, !noBrowser); err != nil {
			fatalf("OAuth 로그인 실패: %v", err)
		}
		fmt.Println("OAuth 로그인 및 토큰 저장이 완료되었습니다.")

	case "import-token":
		if len(os.Args) < 3 {
			fatalf("사용법: gemini-remote import-token <refresh_token 또는 access_token>")
		}
		raw := strings.TrimSpace(os.Args[2])
		if err := authMgr.ImportToken(ctx, raw); err != nil {
			fatalf("토큰 가져오기 실패: %v", err)
		}
		fmt.Println("토큰 저장 및 갱신 검증이 완료되었습니다.")

	case "register":
		d := daemon.New(cfg, authMgr)
		res, err := d.RobinClient().RegisterDevice(ctx)
		if err != nil {
			fatalf("RegisterDevice 실패: %v", err)
		}
		fmt.Printf("기기 등록 완료:\n  device_id:        %s\n  device_name:      %s\n  target_surface:   %d (GEMINI_MACOS)\n  delivery_address: %s\n",
			cfg.DeviceID, cfg.DeviceName, res.TargetSurface, res.DeliveryAddress)

	case "unregister":
		d := daemon.New(cfg, authMgr)
		if err := d.RobinClient().UnregisterDevice(ctx); err != nil {
			fatalf("UnregisterDevice 실패: %v", err)
		}
		fmt.Printf("기기 등록 해제 완료 (device_id=%s)\n", cfg.DeviceID)

	case "devices":
		d := daemon.New(cfg, authMgr)
		devs, err := d.RobinClient().ListDevices(ctx)
		if err != nil {
			fatalf("ListDevices 실패: %v", err)
		}
		if len(devs) == 0 {
			fmt.Println("등록된 원격 데스크톱 기기가 없습니다.")
			return
		}
		for i, dev := range devs {
			fmt.Printf("[%d] %s (device_id=%s, remote=%v, tools=%d)\n",
				i+1, dev.DeviceName, dev.DeviceID, dev.RemoteEnabled, dev.ToolCount)
			for _, f := range dev.Folders {
				fmt.Printf("    - folder: %s\n", f)
			}
		}

	case "serve":
		d := daemon.New(cfg, authMgr)
		if err := d.Serve(ctx); err != nil && ctx.Err() == nil {
			fatalf("데몬 비정상 종료: %v", err)
		}

	case "folders":
		handleFolders(cfg, os.Args[2:])

	case "service":
		handleService(os.Args[2:])

	case "test-tool":
		handleTestTool(ctx, cfg, authMgr, os.Args[2:])

	case "status":
		cfgPath, _ := config.ConfigPath()
		tokPath, _ := config.TokenPath()
		fmt.Printf("설정 파일: %s\n토큰 파일: %s\n기기 ID:   %s\n기기 이름: %s\n연결 폴더: %v\n",
			cfgPath, tokPath, cfg.DeviceID, cfg.DeviceName, cfg.ConnectedFolders)

	default:
		printUsage()
		os.Exit(1)
	}
}

func handleFolders(cfg *config.Config, args []string) {
	if len(args) == 0 || args[0] == "list" {
		for _, f := range cfg.ConnectedFolders {
			fmt.Println(f)
		}
		return
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			fatalf("사용법: gemini-remote folders add <경로>")
		}
		abs, err := filepath.Abs(args[1])
		if err != nil {
			fatalf("잘못된 경로: %v", err)
		}
		for _, existing := range cfg.ConnectedFolders {
			if existing == abs {
				fmt.Println("이미 등록된 폴더입니다:", abs)
				return
			}
		}
		cfg.ConnectedFolders = append(cfg.ConnectedFolders, abs)
		if err := cfg.Save(); err != nil {
			fatalf("설정 저장 실패: %v", err)
		}
		fmt.Println("연결 폴더 추가 완료:", abs)
	case "remove":
		if len(args) < 2 {
			fatalf("사용법: gemini-remote folders remove <경로>")
		}
		abs, _ := filepath.Abs(args[1])
		var next []string
		for _, existing := range cfg.ConnectedFolders {
			if existing != abs && existing != args[1] {
				next = append(next, existing)
			}
		}
		cfg.ConnectedFolders = next
		if err := cfg.Save(); err != nil {
			fatalf("설정 저장 실패: %v", err)
		}
		fmt.Println("연결 폴더 제거 완료:", abs)
	default:
		fatalf("알 수 없는 folders 하위 명령: %s", args[0])
	}
}

func handleService(args []string) {
	if len(args) == 0 {
		fatalf("사용법: gemini-remote service [install|uninstall|status]")
	}
	switch args[0] {
	case "install":
		p, err := daemon.InstallSystemdUserService()
		if err != nil {
			fmt.Fprintf(os.Stderr, "주의: %v\n", err)
			return
		}
		fmt.Println("systemd 사용자 서비스 설치 및 시작 완료:", p)
	case "uninstall":
		if err := daemon.UninstallSystemdUserService(); err != nil {
			fatalf("서비스 제거 실패: %v", err)
		}
		fmt.Println("systemd 사용자 서비스 제거 완료")
	case "status":
		out, _ := daemon.StatusSystemdUserService()
		fmt.Println(out)
	default:
		fatalf("사용법: gemini-remote service [install|uninstall|status]")
	}
}

func handleTestTool(ctx context.Context, cfg *config.Config, authMgr *auth.Manager, args []string) {
	if len(args) < 1 {
		fatalf("사용법: gemini-remote test-tool <tool_name> ['{\"arg\":\"val\"}']")
	}
	toolName := args[0]
	toolArgs := make(map[string]any)
	if len(args) >= 2 {
		if err := json.Unmarshal([]byte(args[1]), &toolArgs); err != nil {
			fatalf("JSON 인자 파싱 실패: %v", err)
		}
	}
	exec := tools.NewExecutor(cfg, authMgr, scotty.NewUploader(), mcp.NewRegistry(cfg))
	res := exec.ExecuteCall(ctx, tools.CallRequest{
		ID:   "local-test-1",
		Name: toolName,
		Args: toolArgs,
	}, false)
	enc, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(enc))
}

func printUsage() {
	fmt.Println(`gemini-remote - Linux용 Gemini Android 원격 제어 브리지 데몬 (macOS Gemini.app 호환)

명령어:
  gemini-remote extract-keys <Gemini.app>   macOS Gemini.app 바이너리에서 OAuth 자격증명 자동 추출
  gemini-remote login [--no-browser]        Google 계정 OAuth 2.0 PKCE 로그인
  gemini-remote import-token <token>        Refresh Token 또는 Access Token 직접 등록
  gemini-remote register                    현재 리눅스 머신 및 19개 로컬 도구를 Robin 서버에 등록
  gemini-remote unregister                  기기 등록 해제
  gemini-remote devices                     계정에 등록된 원격 데스크톱 기기 목록 조회
  gemini-remote serve                       원격 제어 수신 데몬 실행 (Punctual + ExecuteRemoteTools)
  gemini-remote folders [list|add|remove]   허용 워크스페이스 폴더 조회/추가/제거
  gemini-remote service [install|uninstall|status]  systemd 사용자 서비스 관리
  gemini-remote test-tool <name> [json]     로컬 도구(19종) 단독 실행 테스트
  gemini-remote status                      현재 기기 ID 및 설정 상태 출력`)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "오류: "+format+"\n", args...)
	os.Exit(1)
}
