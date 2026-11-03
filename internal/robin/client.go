package robin

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"github.com/sam/gemini-remote-linux/internal/auth"
	"github.com/sam/gemini-remote-linux/internal/config"
	"github.com/sam/gemini-remote-linux/internal/mcp"
	"github.com/sam/gemini-remote-linux/internal/proto"
	"github.com/sam/gemini-remote-linux/internal/tools"
)

const (
	DefaultHost       = "robinfrontend-pa.googleapis.com"
	BundleID          = "com.google.GeminiMacOS"
	AppVersion        = "1.138.0.1315"
	PathRegister      = "/assistant.mobile.robin.notify.GeminiNotifyService/RegisterDevice"
	PathUnregister    = "/assistant.mobile.robin.notify.GeminiNotifyService/UnregisterDevice"
	PathListDevices   = "/assistant.mobile.robin.notify.GeminiNotifyService/ListDevices"
	PathExecuteTools  = "/assistant.mobile.robin.notify.GeminiNotifyService/ExecuteRemoteTools"
	PathPresence      = "/assistant.mobile.robin.RobinService/ReportClientPresence"
)

// Client는 robinfrontend-pa.googleapis.com gRPC/HTTP2 클라이언트다.
type Client struct {
	cfg       *config.Config
	authMgr   *auth.Manager
	executor  *tools.Executor
	mcpReg    *mcp.Registry
	h2Client  *http.Client
	sessionID string
}

func NewClient(cfg *config.Config, authMgr *auth.Manager, executor *tools.Executor, mcpReg *mcp.Registry) *Client {
	tr := &http2.Transport{
		TLSClientConfig: &tls.Config{
			ServerName: DefaultHost,
			NextProtos: []string{"h2"},
			MinVersion: tls.VersionTLS12,
		},
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			d := &tls.Dialer{Config: cfg}
			return d.DialContext(ctx, network, addr)
		},
		ReadIdleTimeout: 30 * time.Second,
		PingTimeout:     15 * time.Second,
	}
	return &Client{
		cfg:       cfg,
		authMgr:   authMgr,
		executor:  executor,
		mcpReg:    mcpReg,
		h2Client:  &http.Client{Transport: tr},
		sessionID: newUUID(),
	}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (c *Client) buildMetadata() []byte {
	return proto.EncodeRequestMetadata("ko-KR", "Asia/Seoul", AppVersion, c.sessionID, strings.ToUpper(c.cfg.DeviceID))
}

// FrameGRPC는 Protobuf 바이트 앞에 5바이트 gRPC Length-Prefixed-Message 헤더를 붙인다.
func FrameGRPC(payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = 0 // uncompressed
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

// ReadGRPCFrame은 io.Reader에서 단일 gRPC 프레임을 읽고 압축(flag=1) 시 자동 해제한다.
func ReadGRPCFrame(r io.Reader) ([]byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length > 64*1024*1024 {
		return nil, fmt.Errorf("비정상적으로 큰 gRPC 프레임 길이: %d", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	if hdr[0] == 1 {
		if gr, err := gzip.NewReader(bytes.NewReader(buf)); err == nil {
			defer gr.Close()
			if decomp, err := io.ReadAll(gr); err == nil {
				return decomp, nil
			}
		}
		if zr, err := zlib.NewReader(bytes.NewReader(buf)); err == nil {
			defer zr.Close()
			if decomp, err := io.ReadAll(zr); err == nil {
				return decomp, nil
			}
		}
		fr := flate.NewReader(bytes.NewReader(buf))
		defer fr.Close()
		if decomp, err := io.ReadAll(fr); err == nil {
			return decomp, nil
		}
		return nil, fmt.Errorf("압축된 gRPC 프레임 해제 실패 (len=%d, hex=%x)", len(buf), buf)
	} else if hdr[0] != 0 {
		return nil, fmt.Errorf("알 수 없는 gRPC 프레임 플래그: %d", hdr[0])
	}
	return buf, nil
}

// invokeUnary는 HTTP/2 gRPC Unary 호출을 수행하고 실패 시 OnePlatform HTTP/Protobuf(/$rpc/...)로 폴백한다.
func (c *Client) invokeUnary(ctx context.Context, rpcPath string, reqPayload []byte) ([]byte, error) {
	token, err := c.authMgr.AccessToken(ctx)
	if err != nil {
		return nil, err
	}

	// 1차 시도: 표준 HTTP/2 gRPC 프레이밍
	grpcURL := "https://" + DefaultHost + rpcPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, grpcURL, bytes.NewReader(FrameGRPC(reqPayload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Ios-Bundle-Identifier", BundleID)
	req.Header.Set("Accept-Language", "ko-KR")
	req.Header.Set("User-Agent", "grpc-c++/1.62.0 grpc-objc/1.62.0")

	resp, err := c.h2Client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		ct := resp.Header.Get("Content-Type")
		if resp.StatusCode == http.StatusOK && strings.HasPrefix(ct, "application/grpc") {
			msg, rerr := ReadGRPCFrame(resp.Body)
			grpcStatus := resp.Trailer.Get("Grpc-Status")
			if grpcStatus == "" {
				grpcStatus = resp.Header.Get("Grpc-Status")
			}
			if rerr == nil && (grpcStatus == "" || grpcStatus == "0") {
				return msg, nil
			}
			if grpcStatus != "" && grpcStatus != "0" {
				grpcMsg := resp.Trailer.Get("Grpc-Message")
				if grpcMsg == "" {
					grpcMsg = resp.Header.Get("Grpc-Message")
				}
				return nil, fmt.Errorf("gRPC 오류 (status=%s, msg=%s)", grpcStatus, grpcMsg)
			}
		}
	}

	// 2차 폴백: Google OnePlatform `/$rpc/...` (application/x-protobuf)
	fallbackURL := "https://" + DefaultHost + "/$rpc" + rpcPath
	fbReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fallbackURL, bytes.NewReader(reqPayload))
	if err != nil {
		return nil, err
	}
	fbReq.Header.Set("Content-Type", "application/x-protobuf")
	fbReq.Header.Set("Accept", "application/x-protobuf")
	fbReq.Header.Set("Authorization", "Bearer "+token)
	fbReq.Header.Set("X-Ios-Bundle-Identifier", BundleID)
	fbReq.Header.Set("Accept-Language", "ko-KR")

	fbResp, err := c.h2Client.Do(fbReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC 및 OnePlatform 호출 실패 (%s): %w", rpcPath, err)
	}
	defer fbResp.Body.Close()
	body, _ := io.ReadAll(fbResp.Body)
	if fbResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("RPC %s 실패 (HTTP %d): %s", rpcPath, fbResp.StatusCode, string(body))
	}
	return body, nil
}

// RegisterDevice는 현재 리눅스 머신과 19개 도구 스키마, 연결 폴더, 부트스트랩 파일, 내장 MCP 서버를 등록한다.
func (c *Client) RegisterDevice(ctx context.Context) (*proto.RegisterDeviceResult, error) {
	bootstrapMap := c.cfg.CollectBootstrapFiles()
	var protoBootstrap []proto.BootstrapFileInfo
	for filename, content := range bootstrapMap {
		protoBootstrap = append(protoBootstrap, proto.BootstrapFileInfo{
			Filename:         filename,
			LastModifiedTime: time.Now().UTC(),
			Content:          string(content),
		})
	}

	bundledMcp := []proto.BundledMcpServerInfo{
		{
			ServerID: "git",
			Enabled:  true,
		},
		{
			ServerID: "obsidian",
			Enabled:  true,
		},
	}

	desktopInfo := proto.EncodeDesktopDeviceInfo(proto.DesktopDeviceParams{
		DeviceID:         strings.ToUpper(c.cfg.DeviceID),
		DeviceName:       c.cfg.DeviceName,
		ToolDeclarations: c.executor.Declarations(),
		ConnectedFolders: c.cfg.ConnectedFolders,
		BootstrapFiles:   protoBootstrap,
		BundledMcp:       bundledMcp,
	})

	reqBytes := proto.EncodeRegisterDeviceRequest(c.buildMetadata(), AppVersion, desktopInfo)
	respBytes, err := c.invokeUnary(ctx, PathRegister, reqBytes)
	if err != nil {
		return nil, err
	}
	return proto.DecodeRegisterDeviceResponse(respBytes)
}

// UnregisterDevice는 기기 등록을 해제한다.
func (c *Client) UnregisterDevice(ctx context.Context) error {
	reqBytes := proto.EncodeUnregisterDeviceRequest(c.buildMetadata(), strings.ToUpper(c.cfg.DeviceID))
	_, err := c.invokeUnary(ctx, PathUnregister, reqBytes)
	return err
}

// ListDevices는 현재 계정에 등록된 원격 데스크톱 기기 목록을 조회한다.
func (c *Client) ListDevices(ctx context.Context) ([]proto.RegisteredDeviceSummary, error) {
	reqBytes := proto.EncodeListDevicesRequest(c.buildMetadata())
	respBytes, err := c.invokeUnary(ctx, PathListDevices, reqBytes)
	if err != nil {
		return nil, err
	}
	return proto.DecodeListDevicesResponse(respBytes)
}

// ReportClientPresence는 기기의 온라인 활성 상태를 보고한다.
func (c *Client) ReportClientPresence(ctx context.Context) error {
	reqBytes := proto.EncodeReportClientPresenceRequest(c.buildMetadata(), strings.ToUpper(c.cfg.DeviceID))
	_, err := c.invokeUnary(ctx, PathPresence, reqBytes)
	return err
}

// ExecuteRemoteToolsSession은 WakeUpEvent 수신 시 양방향 gRPC 스트림을 열어 도구 호출을 처리하고,
// 멀티턴 대화를 위해 서버가 활성 세션을 유지하는 동안 스트림을 자동 갱신한다.
func (c *Client) ExecuteRemoteToolsSession(ctx context.Context, sessionRoutingKey, routingToken string) error {
	const (
		maxErrorRetries = 3
		maxIdleRenewals = 15 // 유휴 스트림 최대 갱신 횟수 (약 60분, 이후에는 ClientSync 힌트나 WakeUp으로 재진입)
	)
	errorAttempt := 0
	idleRenewals := 0
	currentToken := routingToken

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		outcome, hadToolCalls, grpcStatus, err := c.runSingleRemoteToolStream(ctx, sessionRoutingKey, currentToken)
		if ctx.Err() != nil {
			return nil
		}
		if hadToolCalls {
			idleRenewals = 0
		}

		switch outcome {
		case streamNoActiveSession:
			fmt.Printf("[robin] 서버 활성 세션 종료 확인 -> 세션 루프 종료 (routing_key=%s)\n", sessionRoutingKey)
			return nil
		case streamRenewAcrossTurns:
			errorAttempt = 0
			if !hadToolCalls {
				idleRenewals++
				if idleRenewals > maxIdleRenewals {
					fmt.Printf("[robin] 장시간 유휴 세션 대기 종료 -> 다음 신호 대기 (routing_key=%s)\n", sessionRoutingKey)
					return nil
				}
			}
			fmt.Printf("[robin] 멀티턴 세션 유지를 위해 스트림 재연결 대기 (routing_key=%s, idle=%d/%d)\n", sessionRoutingKey, idleRenewals, maxIdleRenewals)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(250 * time.Millisecond):
			}
			continue
		default:
			if grpcStatus == "5" { // NOT_FOUND: 서버 세션 만료
				fmt.Printf("[robin] 서버 세션 만료(NOT_FOUND) -> 세션 루프 종료 (routing_key=%s)\n", sessionRoutingKey)
				return nil
			}
			if grpcStatus == "8" && currentToken != "" { // RESOURCE_EXHAUSTED: routing_token 폐기 후 재시도
				fmt.Printf("[robin] RESOURCE_EXHAUSTED 수신 -> routing_token 제외 후 재연결 (routing_key=%s)\n", sessionRoutingKey)
				currentToken = ""
			}
			errorAttempt++
			if errorAttempt > maxErrorRetries {
				return fmt.Errorf("최대 재연결 횟수(%d) 초과 (마지막 오류: %w)", maxErrorRetries, err)
			}
			backoff := time.Duration(800*(1<<(errorAttempt-1))) * time.Millisecond
			fmt.Printf("[robin] 스트림 오류(%v), %v 후 재연결 시도 (%d/%d)\n", err, backoff, errorAttempt, maxErrorRetries)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
		}
	}
}

type streamOutcome int

const (
	streamError streamOutcome = iota
	streamRenewAcrossTurns
	streamNoActiveSession
)

func (c *Client) runSingleRemoteToolStream(ctx context.Context, sessionRoutingKey, routingToken string) (streamOutcome, bool, string, error) {
	token, err := c.authMgr.AccessToken(ctx)
	if err != nil {
		return streamError, false, "", err
	}

	pr, pw := io.Pipe()
	defer pw.Close()

	var writeMu sync.Mutex
	sendFrame := func(payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err := pw.Write(FrameGRPC(payload))
		return err
	}

	grpcURL := "https://" + DefaultHost + PathExecuteTools
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, grpcURL, pr)
	if err != nil {
		return streamError, false, "", err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Ios-Bundle-Identifier", BundleID)
	req.Header.Set("Accept-Language", "ko-KR")
	req.Header.Set("User-Agent", "grpc-c++/1.62.0 grpc-objc/1.62.0")

	// 첫 메시지(RemoteToolConnectRequest)를 비동기로 즉시 전송
	errCh := make(chan error, 1)
	go func() {
		connMsg := proto.EncodeRemoteToolConnectRequest(
			c.buildMetadata(),
			sessionRoutingKey,
			strings.ToUpper(c.cfg.DeviceID),
			routingToken,
		)
		if werr := sendFrame(connMsg); werr != nil {
			errCh <- fmt.Errorf("RemoteToolConnectRequest 전송 실패: %w", werr)
		}
	}()

	resp, err := c.h2Client.Do(req)
	if err != nil {
		return streamError, false, "", fmt.Errorf("ExecuteRemoteTools 스트림 연결 실패: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return streamError, false, "", fmt.Errorf("ExecuteRemoteTools HTTP %d: %s", resp.StatusCode, string(body))
	}

	hadHealthyActivity := false
	hadToolCalls := false

	// macOS Gemini.app 동일 사양: 240초(4분) 선제적 스트림 롤오버 워치독
	var stateMu sync.Mutex
	inToolExecution := false
	rolloverTriggered := false
	rolloverDeferred := false

	rolloverTimer := time.AfterFunc(240*time.Second, func() {
		stateMu.Lock()
		if inToolExecution {
			rolloverDeferred = true
			stateMu.Unlock()
			return
		}
		rolloverTriggered = true
		stateMu.Unlock()
		fmt.Printf("[robin] 240초 선제적 스트림 롤오버 워치독 작동 -> 스트림 갱신 (routing_key=%s)\n", sessionRoutingKey)
		_ = resp.Body.Close()
	})
	defer rolloverTimer.Stop()

	for {
		select {
		case werr := <-errCh:
			return streamError, hadToolCalls, "", werr
		default:
		}

		frameBytes, err := ReadGRPCFrame(resp.Body)
		stateMu.Lock()
		rolledOver := rolloverTriggered
		stateMu.Unlock()
		if rolledOver {
			return streamRenewAcrossTurns, hadToolCalls, "0", nil
		}
		if err == io.EOF {
			grpcStatus := resp.Trailer.Get("Grpc-Status")
			if grpcStatus == "" {
				grpcStatus = resp.Header.Get("Grpc-Status")
			}
			grpcMsg := resp.Trailer.Get("Grpc-Message")
			if grpcMsg == "" {
				grpcMsg = resp.Header.Get("Grpc-Message")
			}
			if grpcStatus != "" && grpcStatus != "0" {
				if hadHealthyActivity && (grpcStatus == "1" || grpcStatus == "4" || grpcStatus == "13" || grpcStatus == "14") {
					// 정상 연결 이후 스트림 유휴 타임아웃/갱신 종료 시 새 스트림으로 갱신
					return streamRenewAcrossTurns, hadToolCalls, grpcStatus, nil
				}
				return streamError, hadToolCalls, grpcStatus, fmt.Errorf("ExecuteRemoteTools 스트림 종료 (status=%s, msg=%s)", grpcStatus, grpcMsg)
			}
			if hadHealthyActivity {
				return streamRenewAcrossTurns, hadToolCalls, "0", nil
			}
			return streamNoActiveSession, hadToolCalls, "0", nil
		}
		if err != nil {
			return streamError, hadToolCalls, "", fmt.Errorf("ExecuteRemoteTools 프레임 수신 오류: %w", err)
		}

		fmt.Printf("[robin] ExecuteRemoteTools 프레임 수신 (len=%d, hex=%x)\n", len(frameBytes), frameBytes)

		ev, err := proto.DecodeExecuteRemoteToolsResponse(frameBytes)
		if err != nil {
			fmt.Printf("[robin] 응답 디코딩 경고: %v\n", err)
			continue
		}

		if ev.ConnectAck {
			fmt.Printf("[robin] 원격 세션 연결 응답 (connected=%v, session_id=%s)\n", ev.Connected, ev.SessionID)
			if !ev.Connected {
				return streamNoActiveSession, hadToolCalls, "0", nil
			}
			hadHealthyActivity = true
		}

		if len(ev.FunctionCalls) > 0 {
			hadHealthyActivity = true
			hadToolCalls = true
			stateMu.Lock()
			inToolExecution = true
			stateMu.Unlock()

			var results []proto.FunctionResponseInput
			for i, fc := range ev.FunctionCalls {
				fmt.Printf("[robin] 도구 실행 요청: %s (id=%s, args=%v)\n", fc.Name, fc.ID, fc.Args)
				skipPost := i < len(ev.FunctionCalls)-1
				callResp := c.executor.ExecuteCall(ctx, tools.CallRequest{
					ID:   fc.ID,
					Name: fc.Name,
					Args: fc.Args,
				}, skipPost)
				if errVal, ok := callResp.Response["error"]; ok {
					fmt.Printf("[robin] 도구 실행 오류 (%s): %v\n", fc.Name, errVal)
				}
				var atts []proto.AttachmentInput
				for _, a := range callResp.Attachments {
					attName := a.Name
					if attName == "" {
						attName = fc.Name
					}
					atts = append(atts, proto.AttachmentInput{
						BlobID:     a.BlobID,
						FileType:   a.FileType,
						MimeType:   a.MimeType,
						FileSource: a.FileSource,
						Name:       attName,
					})
				}
				results = append(results, proto.FunctionResponseInput{
					Name:        callResp.Name,
					ID:          callResp.ID,
					Response:    callResp.Response,
					Attachments: atts,
				})
			}

			resMsg := proto.EncodeRemoteToolResultRequest(c.buildMetadata(), results)
			if err := sendFrame(resMsg); err != nil {
				stateMu.Lock()
				inToolExecution = false
				stateMu.Unlock()
				return streamError, hadToolCalls, "", fmt.Errorf("도구 실행 결과 전송 실패: %w", err)
			}
			fmt.Printf("[robin] 도구 실행 결과 %d건 응답 완료\n", len(results))

			stateMu.Lock()
			inToolExecution = false
			deferred := rolloverDeferred
			stateMu.Unlock()
			if deferred {
				fmt.Printf("[robin] 지연된 240초 롤오버 실행 -> 스트림 갱신 (routing_key=%s)\n", sessionRoutingKey)
				return streamRenewAcrossTurns, hadToolCalls, "0", nil
			}
		}

		if ev.SessionComplete {
			fmt.Printf("[robin] 턴 도구 실행 완료 수신 (conversation_id=%s) -> 다음 턴 대비 스트림 갱신\n", ev.ConversationID)
			return streamRenewAcrossTurns, hadToolCalls, "0", nil
		}
	}
}
