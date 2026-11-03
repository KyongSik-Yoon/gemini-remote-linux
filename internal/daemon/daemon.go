package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/sam/gemini-remote-linux/internal/auth"
	"github.com/sam/gemini-remote-linux/internal/config"
	"github.com/sam/gemini-remote-linux/internal/mcp"
	"github.com/sam/gemini-remote-linux/internal/proto"
	"github.com/sam/gemini-remote-linux/internal/punctual"
	"github.com/sam/gemini-remote-linux/internal/robin"
	"github.com/sam/gemini-remote-linux/internal/scotty"
	"github.com/sam/gemini-remote-linux/internal/tools"
)

// Daemon은 RegisterDevice, 주기적 ReportClientPresence, Punctual WakeUp 수신, ExecuteRemoteTools 스트림을 총괄한다.
type Daemon struct {
	cfg      *config.Config
	authMgr  *auth.Manager
	robinCli *robin.Client
	watcher  *punctual.Watcher

	sessionMu      sync.Mutex
	activeSessions map[string]*sessionEntry
	lastHintProbe  map[string]time.Time
}

type sessionEntry struct {
	id     uint64
	cancel context.CancelFunc
}

var nextSessionSeq uint64

func New(cfg *config.Config, authMgr *auth.Manager) *Daemon {
	uploader := scotty.NewUploader()
	mcpReg := mcp.NewRegistry(cfg)
	executor := tools.NewExecutor(cfg, authMgr, uploader, mcpReg)
	robinCli := robin.NewClient(cfg, authMgr, executor, mcpReg)
	executor.SetOnFoldersChanged(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := robinCli.RegisterDevice(ctx); err != nil {
			fmt.Printf("[daemon] 폴더 변경 후 RegisterDevice 경고: %v\n", err)
		} else {
			fmt.Printf("[daemon] 연결 폴더 변경사항 서버 등록 완료 (%v)\n", cfg.ConnectedFolders)
		}
	})
	watcher := punctual.NewWatcher(authMgr, cfg.DeviceID)

	return &Daemon{
		cfg:            cfg,
		authMgr:        authMgr,
		robinCli:       robinCli,
		watcher:        watcher,
		activeSessions: make(map[string]*sessionEntry),
		lastHintProbe:  make(map[string]time.Time),
	}
}

func (d *Daemon) RobinClient() *robin.Client {
	return d.robinCli
}

// Serve는 데몬 메인 루프를 실행한다.
func (d *Daemon) Serve(ctx context.Context) error {
	fmt.Printf("[daemon] 리눅스 원격 제어 데몬 시작 (device_id=%s, name=%s)\n", d.cfg.DeviceID, d.cfg.DeviceName)

	res, err := d.robinCli.RegisterDevice(ctx)
	if err != nil {
		return fmt.Errorf("초기 RegisterDevice 실패: %w", err)
	}
	fmt.Printf("[daemon] 기기 등록 완료 (surface=%d, delivery_address=%s)\n", res.TargetSurface, res.DeliveryAddress)

	_ = d.robinCli.ReportClientPresence(ctx)

	// 주기적 Presence 보고 및 재등록 루프
	go func() {
		presenceTicker := time.NewTicker(2 * time.Minute)
		registerTicker := time.NewTicker(30 * time.Minute)
		defer presenceTicker.Stop()
		defer registerTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-presenceTicker.C:
				if err := d.robinCli.ReportClientPresence(ctx); err != nil {
					fmt.Printf("[daemon] ReportClientPresence 경고: %v\n", err)
				}
			case <-registerTicker.C:
				if _, err := d.robinCli.RegisterDevice(ctx); err != nil {
					fmt.Printf("[daemon] 주기적 RegisterDevice 경고: %v\n", err)
				}
			}
		}
	}()

	// Punctual WebChannel 실시간 감시 루프
	return d.watcher.Run(ctx, func(ctx context.Context, ev *proto.WakeUpEvent) {
		if ev.SessionRoutingKey == "" {
			return
		}
		if ev.IsClientSyncHint {
			sessionCtx, seq, ok := d.tryStartSessionFromHint(ctx, ev.SessionRoutingKey)
			if !ok {
				return
			}
			defer d.releaseSession(ev.SessionRoutingKey, seq)
			fmt.Printf("[daemon] ClientSync 대화방 힌트로 세션 복구 시도 (routing_key=%s, seq=%d)\n", ev.SessionRoutingKey, seq)
			if err := d.robinCli.ExecuteRemoteToolsSession(sessionCtx, ev.SessionRoutingKey, ""); err != nil {
				fmt.Printf("[daemon] 대화방 힌트 세션 종료 (routing_key=%s): %v\n", ev.SessionRoutingKey, err)
			}
			return
		}
		sessionCtx, seq := d.startOrReplaceSession(ctx, ev.SessionRoutingKey)
		defer d.releaseSession(ev.SessionRoutingKey, seq)
		fmt.Printf("[daemon] WakeUp 수신 -> 원격 도구 세션 시작 (routing_key=%s, seq=%d)\n", ev.SessionRoutingKey, seq)
		if err := d.robinCli.ExecuteRemoteToolsSession(sessionCtx, ev.SessionRoutingKey, ev.RoutingToken); err != nil {
			fmt.Printf("[daemon] 원격 도구 세션 오류 (routing_key=%s): %v\n", ev.SessionRoutingKey, err)
		}
	})
}

func (d *Daemon) tryStartSessionFromHint(parent context.Context, routingKey string) (context.Context, uint64, bool) {
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	if prev, exists := d.activeSessions[routingKey]; exists && prev != nil {
		return nil, 0, false
	}
	if last, ok := d.lastHintProbe[routingKey]; ok && time.Since(last) < 3*time.Second {
		return nil, 0, false
	}
	d.lastHintProbe[routingKey] = time.Now()
	nextSessionSeq++
	seq := nextSessionSeq
	sessionCtx, cancel := context.WithCancel(parent)
	d.activeSessions[routingKey] = &sessionEntry{
		id:     seq,
		cancel: cancel,
	}
	return sessionCtx, seq, true
}

func (d *Daemon) startOrReplaceSession(parent context.Context, routingKey string) (context.Context, uint64) {
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	if prev, exists := d.activeSessions[routingKey]; exists && prev != nil {
		fmt.Printf("[daemon] 새 WakeUp 수신 -> 기존 대기 스트림 교체 (routing_key=%s)\n", routingKey)
		prev.cancel()
	}
	nextSessionSeq++
	seq := nextSessionSeq
	sessionCtx, cancel := context.WithCancel(parent)
	d.activeSessions[routingKey] = &sessionEntry{
		id:     seq,
		cancel: cancel,
	}
	return sessionCtx, seq
}

func (d *Daemon) releaseSession(routingKey string, seq uint64) {
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	if cur, exists := d.activeSessions[routingKey]; exists && cur.id == seq {
		cur.cancel()
		delete(d.activeSessions, routingKey)
	}
}

// InstallSystemdUserService는 ~/.config/systemd/user/gemini-remote.service 유닛을 생성하고 활성화한다.
func InstallSystemdUserService() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return "", err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return "", err
	}
	unitPath := filepath.Join(unitDir, "gemini-remote.service")

	unitContent := fmt.Sprintf(`[Unit]
Description=Gemini Remote Control Daemon for Linux (macOS Gemini.app compatible)
After=network-online.target graphical-session.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s serve
Restart=always
RestartSec=5
Environment=PATH=%s

[Install]
WantedBy=default.target
`, exePath, os.Getenv("PATH"))

	if err := os.WriteFile(unitPath, []byte(unitContent), 0o644); err != nil {
		return "", err
	}

	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", "gemini-remote.service").CombinedOutput(); err != nil {
		return unitPath, fmt.Errorf("유닛 파일 생성 완료(%s), systemctl 활성화 경고: %v (%s)", unitPath, err, string(out))
	}
	return unitPath, nil
}

// UninstallSystemdUserService는 systemd 사용자 서비스를 중지하고 제거한다.
func UninstallSystemdUserService() error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", "gemini-remote.service").Run()
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	unitPath := filepath.Join(home, ".config", "systemd", "user", "gemini-remote.service")
	_ = os.Remove(unitPath)
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

// StatusSystemdUserService는 systemd 사용자 서비스 상태를 반환한다.
func StatusSystemdUserService() (string, error) {
	out, err := exec.Command("systemctl", "--user", "status", "--no-pager", "gemini-remote.service").CombinedOutput()
	return string(out), err
}
