package punctual

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sam/gemini-remote-linux/internal/auth"
	"github.com/sam/gemini-remote-linux/internal/proto"
)

const (
	SignalerBaseURL = "https://signaler-pa.googleapis.com"
	PunctualProject = "assistant-bard"
)

// Handler는 WakeUpEvent가 도착했을 때 호출되는 콜백이다.
type Handler func(ctx context.Context, ev *proto.WakeUpEvent)

// Watcher는 signaler-pa.googleapis.com의 Punctual WebChannel V8 스트림을 구독한다.
type Watcher struct {
	authMgr    *auth.Manager
	deviceID   string
	httpClient *http.Client
}

func NewWatcher(authMgr *auth.Manager, deviceID string) *Watcher {
	return &Watcher{
		authMgr:  authMgr,
		deviceID: deviceID,
		httpClient: &http.Client{
			Timeout: 0, // 스트리밍 BackChannel을 위해 타임아웃 해제
		},
	}
}

// Run은 컨텍스트 취소 시까지 지수 백오프로 Punctual WebChannel 연결을 유지한다.
func (w *Watcher) Run(ctx context.Context, handler Handler) error {
	backoff := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		start := time.Now()
		err := w.watchOnce(ctx, handler)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) > 15*time.Second {
			backoff = 2 * time.Second
		}
		if err == nil {
			fmt.Printf("[punctual] 세션 주기적 갱신(40분) 도래 -> 새 WebChannel 세션 즉시 연결\n")
			continue
		}
		fmt.Printf("[punctual] 스트림 연결 종료 (%v), %v 후 재연결...\n", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (w *Watcher) chooseServer(ctx context.Context, token string) (string, error) {
	reqBytes := proto.EncodeChooseServerRequest(PunctualProject, "bard-client-sync")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, SignalerBaseURL+"/punctual/v1/chooseServer", bytes.NewReader(reqBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept", "application/x-protobuf")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chooseServer HTTP %d: %s", resp.StatusCode, string(body))
	}
	return proto.DecodeChooseServerResponse(body)
}

func (w *Watcher) watchOnce(parentCtx context.Context, handler Handler) error {
	// OAuth 액세스 토큰(1시간 수명) 만료 전 새 MultiWatch 세션을 맺기 위해 40분마다 세션을 선제 회전한다.
	ctx, cancel := context.WithTimeout(parentCtx, 40*time.Minute)
	defer cancel()

	token, err := w.authMgr.AccessToken(ctx)
	if err != nil {
		return err
	}

	gsessionID, err := w.chooseServer(ctx, token)
	if err != nil {
		return fmt.Errorf("chooseServer 실패: %w", err)
	}

	// Step 1: Forward Channel POST로 SID 발급 + MultiWatchRequestBatch 전송
	rid := 10000 + rand.Intn(80000)
	zx := randomZX()
	batchBytes := proto.EncodeMultiWatchRequestBatch(PunctualProject)
	b64Batch := base64.StdEncoding.EncodeToString(batchBytes)
	formBody := fmt.Sprintf("count=1&ofs=0&req0___data__=%s", url.QueryEscape(b64Batch))

	fwdURL := fmt.Sprintf("%s/punctual/multi-watch/channel?VER=8&gsessionid=%s&RID=%d&CVER=22&zx=%s&t=1",
		SignalerBaseURL, url.QueryEscape(gsessionID), rid, zx)

	fwdReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fwdURL, strings.NewReader(formBody))
	if err != nil {
		return err
	}
	fwdReq.Header.Set("Authorization", "Bearer "+token)
	fwdReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	fwdReq.Header.Set("X-Client-Protocol", "webchannel")
	fwdReq.Header.Set("X-WebChannel-Content-Type", "application/x-protobuf")

	fwdResp, err := w.httpClient.Do(fwdReq)
	if err != nil {
		return fmt.Errorf("ForwardChannel 요청 실패: %w", err)
	}
	fwdBody, _ := io.ReadAll(fwdResp.Body)
	fwdResp.Body.Close()
	if fwdResp.StatusCode != http.StatusOK {
		return fmt.Errorf("ForwardChannel HTTP %d: %s", fwdResp.StatusCode, string(fwdBody))
	}

	sid := extractSID(string(fwdBody))
	if sid == "" {
		return fmt.Errorf("ForwardChannel 응답에서 SID 추출 실패: %s", string(fwdBody))
	}
	fmt.Printf("[punctual] WebChannel 세션 연결됨 (gsessionid=%s, SID=%s)\n", gsessionID, sid)

	// Step 2: BackChannel GET 스트리밍 루프
	aid := 0
	for {
		if ctx.Err() != nil {
			if parentCtx.Err() == nil {
				return nil // 40분 주기 계획된 세션 회전
			}
			return parentCtx.Err()
		}
		token, err = w.authMgr.AccessToken(ctx)
		if err != nil {
			return err
		}

		backURL := fmt.Sprintf("%s/punctual/multi-watch/channel?VER=8&gsessionid=%s&SID=%s&RID=rpc&CI=0&AID=%d&TYPE=xmlhttp&zx=%s&t=1",
			SignalerBaseURL, url.QueryEscape(gsessionID), url.QueryEscape(sid), aid, randomZX())

		backReq, err := http.NewRequestWithContext(ctx, http.MethodGet, backURL, nil)
		if err != nil {
			return err
		}
		backReq.Header.Set("Authorization", "Bearer "+token)
		backReq.Header.Set("X-Client-Protocol", "webchannel")
		backReq.Header.Set("X-WebChannel-Content-Type", "application/x-protobuf")

		backResp, err := w.httpClient.Do(backReq)
		if err != nil {
			if ctx.Err() != nil && parentCtx.Err() == nil {
				return nil
			}
			return fmt.Errorf("BackChannel 연결 오류: %w", err)
		}
		if backResp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(backResp.Body)
			backResp.Body.Close()
			return fmt.Errorf("BackChannel HTTP %d: %s", backResp.StatusCode, string(b))
		}

		newAID, streamErr := w.consumeBackChannel(parentCtx, backResp.Body, aid, handler)
		backResp.Body.Close()
		aid = newAID
		if ctx.Err() != nil && parentCtx.Err() == nil {
			return nil
		}
		if streamErr != nil && streamErr != io.EOF {
			return streamErr
		}
	}
}

func (w *Watcher) consumeBackChannel(handlerCtx context.Context, body io.ReadCloser, lastAID int, handler Handler) (int, error) {
	reader := bufio.NewReaderSize(body, 64*1024)
	aid := lastAID

	// WebChannel은 약 25~30초마다 ["noop"] 하트비트를 보내므로 90초간 수신이 없으면 좀비 연결로 간주해 닫는다.
	doneCh := make(chan struct{})
	defer close(doneCh)
	activityCh := make(chan struct{}, 1)
	go func() {
		timer := time.NewTimer(90 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-doneCh:
				return
			case <-activityCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(90 * time.Second)
			case <-timer.C:
				fmt.Printf("[punctual] BackChannel 90초 읽기 타임아웃 감지 -> 스트림 강제 재연결\n")
				_ = body.Close()
				return
			}
		}
	}()

	touchActivity := func() {
		select {
		case activityCh <- struct{}{}:
		default:
		}
	}

	for {
		// WebChannel 청크는 "<길이>\n<JSON 페이로드>" 또는 줄 단위 JSON 배열로 도착한다.
		line, err := reader.ReadString('\n')
		if err != nil {
			return aid, err
		}
		touchActivity()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		var chunk string
		if length, lerr := strconv.Atoi(trimmed); lerr == nil && length > 0 {
			buf := make([]byte, length)
			if _, rerr := io.ReadFull(reader, buf); rerr != nil {
				return aid, rerr
			}
			touchActivity()
			chunk = string(buf)
		} else if strings.HasPrefix(trimmed, "[") {
			chunk = trimmed
		} else {
			continue
		}

		newAID, payloads, shouldStop := ParseWebChannelChunk(chunk)
		if newAID > aid {
			aid = newAID
		}
		reconnectNeeded := shouldStop
		for _, p := range payloads {
			changes, needsReconnect, err := proto.DecodeMultiWatchResponseBatch(p)
			if needsReconnect {
				reconnectNeeded = true
			}
			if err != nil {
				continue
			}
			for _, ch := range changes {
				fmt.Printf("[punctual] 이벤트 수신 (stream=%d, path=%s, len=%d, hex=%x)\n", ch.StreamID, ch.ObjectPath, len(ch.Payload), ch.Payload)
				ev, err := proto.DecodeWakeUpPayload(ch.StreamID, ch.ObjectPath, ch.Payload)
				if err != nil || ev == nil {
					if err != nil {
						fmt.Printf("[punctual] 이벤트 참고: %v\n", err)
					}
					continue
				}
				if ev.IsClientSyncHint {
					fmt.Printf("[punctual] 대화방 갱신 힌트 감지 (routing_key=%s)\n", ev.SessionRoutingKey)
				} else {
					fmt.Printf("[punctual] 원격 실행 트리거 확인 (routing_key=%s, target=%s, task=%s)\n", ev.SessionRoutingKey, ev.TargetDeviceID, ev.ScheduledTaskName)
				}
				if ev.TargetDeviceID != "" && !strings.EqualFold(ev.TargetDeviceID, w.deviceID) {
					continue
				}
				go handler(handlerCtx, ev)
			}
		}
		if reconnectNeeded {
			return aid, fmt.Errorf("서버 스트림 종료 또는 Resync 요청으로 세션 재구독")
		}
	}
}

// ParseWebChannelChunk는 WebChannel JSON 배열 청크에서 마지막 AID, 디코딩된 Protobuf 바이트 목록, 종료 제어 신호 여부를 추출한다.
func ParseWebChannelChunk(chunk string) (int, [][]byte, bool) {
	var outer []any
	if err := json.Unmarshal([]byte(chunk), &outer); err != nil {
		return 0, nil, false
	}
	maxAID := 0
	var out [][]byte
	shouldStop := false

	for _, elem := range outer {
		pair, ok := elem.([]any)
		if !ok || len(pair) < 2 {
			continue
		}
		if f, ok := pair[0].(float64); ok && int(f) > maxAID {
			maxAID = int(f)
		}
		inner, ok := pair[1].([]any)
		if !ok || len(inner) == 0 {
			continue
		}
		if tag, ok := inner[0].(string); ok && (tag == "stop" || tag == "close") {
			shouldStop = true
			continue
		}
		for _, item := range inner {
			switch val := item.(type) {
			case string:
				if val == "noop" || val == "c" {
					continue
				}
				if decoded, err := base64.StdEncoding.DecodeString(val); err == nil && len(decoded) > 0 {
					out = append(out, decoded)
				}
			case map[string]any:
				if raw, ok := val["__data__"].(string); ok {
					if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && len(decoded) > 0 {
						out = append(out, decoded)
					}
				}
			}
		}
	}
	return maxAID, out, shouldStop
}

func extractSID(body string) string {
	var outer []any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &outer); err != nil {
			continue
		}
		for _, elem := range outer {
			pair, ok := elem.([]any)
			if !ok || len(pair) < 2 {
				continue
			}
			inner, ok := pair[1].([]any)
			if !ok || len(inner) < 2 {
				continue
			}
			if tag, ok := inner[0].(string); ok && tag == "c" {
				if sid, ok := inner[1].(string); ok {
					return sid
				}
			}
		}
	}
	return ""
}

func randomZX() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}
