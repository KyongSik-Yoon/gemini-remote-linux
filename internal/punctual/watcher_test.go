package punctual

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/sam/gemini-remote-linux/internal/proto"
)

func TestParseWebChannelChunk(t *testing.T) {
	dispatch := proto.NewBuilder()
	dispatch.AppendString(1, "session_key_777")
	dispatch.AppendString(2, "routing_tok_888")

	wakeup := proto.NewBuilder()
	wakeup.AppendMessage(1, dispatch.Bytes())

	objPath := proto.NewBuilder()
	objPath.AppendString(4, "remote-tool-wakeup")

	objChange := proto.NewBuilder()
	objChange.AppendBytes(2, wakeup.Bytes())
	objChange.AppendMessage(6, objPath.Bytes())

	watchResp := proto.NewBuilder()
	watchResp.AppendMessage(2, objChange.Bytes())

	multiResp := proto.NewBuilder()
	multiResp.AppendInt64Always(1, 2)
	multiResp.AppendMessage(2, watchResp.Bytes())

	batch := proto.NewBuilder()
	batch.AppendMessage(1, multiResp.Bytes())

	b64 := base64.StdEncoding.EncodeToString(batch.Bytes())
	chunk := fmt.Sprintf(`[[0, ["c", "SID_12345", null, 8]], [1, ["%s"]]]`, b64)

	if sid := extractSID(chunk); sid != "SID_12345" {
		t.Fatalf("SID 추출 실패: %s", sid)
	}

	aid, payloads, shouldStop := ParseWebChannelChunk(chunk)
	if aid != 1 || len(payloads) != 1 || shouldStop {
		t.Fatalf("청크 파싱 결과 불일치: aid=%d, payloads=%d, stop=%v", aid, len(payloads), shouldStop)
	}

	changes, needsReconnect, err := proto.DecodeMultiWatchResponseBatch(payloads[0])
	if err != nil || len(changes) != 1 || needsReconnect {
		t.Fatalf("MultiWatchResponseBatch 디코딩 실패: %v, len=%d, reconnect=%v", err, len(changes), needsReconnect)
	}
	ev, err := proto.DecodeWakeUpPayload(changes[0].StreamID, changes[0].ObjectPath, changes[0].Payload)
	if err != nil {
		t.Fatalf("WakeUpPayload 디코딩 실패: %v", err)
	}
	if ev.SessionRoutingKey != "session_key_777" || ev.RoutingToken != "routing_tok_888" {
		t.Fatalf("디코딩된 이벤트 값 불일치: %+v", ev)
	}
}
