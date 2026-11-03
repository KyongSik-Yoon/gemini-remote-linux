package proto

import (
	"testing"
)

func TestStructRoundTrip(t *testing.T) {
	in := map[string]any{
		"command":   "ls -la",
		"exit_code": 0,
		"success":   true,
		"tags":      []any{"linux", "wayland"},
		"nested": map[string]any{
			"key": "val",
		},
	}
	encoded := EncodeStruct(in)
	decoded := DecodeStruct(encoded)

	if decoded["command"] != "ls -la" {
		t.Fatalf("command 불일치: %v", decoded["command"])
	}
	if decoded["exit_code"] != float64(0) {
		t.Fatalf("exit_code 불일치: %v", decoded["exit_code"])
	}
	if decoded["success"] != true {
		t.Fatalf("success 불일치: %v", decoded["success"])
	}
}

func TestExecuteRemoteToolsDecode(t *testing.T) {
	// ClientFunctionCallArgs.FunctionCall { 1: "run_command", 2: "call_123", 3: Struct{"command": "uname -a"} }
	fc := NewBuilder()
	fc.AppendString(1, "run_command")
	fc.AppendString(2, "call_123")
	fc.AppendMessage(3, EncodeStruct(map[string]any{"command": "uname -a"}))

	argsMsg := NewBuilder()
	argsMsg.AppendMessage(1, fc.Bytes())

	resp := NewBuilder()
	resp.AppendMessage(2, argsMsg.Bytes())
	resp.AppendString(5, "conv_999")

	ev, err := DecodeExecuteRemoteToolsResponse(resp.Bytes())
	if err != nil {
		t.Fatalf("디코딩 실패: %v", err)
	}
	if len(ev.FunctionCalls) != 1 {
		t.Fatalf("FunctionCalls 개수 불일치: %d", len(ev.FunctionCalls))
	}
	if ev.FunctionCalls[0].Name != "run_command" || ev.FunctionCalls[0].ID != "call_123" {
		t.Fatalf("FunctionCall 메타 불일치: %+v", ev.FunctionCalls[0])
	}
	if ev.FunctionCalls[0].Args["command"] != "uname -a" {
		t.Fatalf("FunctionCall 인자 불일치: %+v", ev.FunctionCalls[0].Args)
	}
}

func TestWakeUpPayloadDecode(t *testing.T) {
	dispatch := NewBuilder()
	dispatch.AppendString(1, "rk_abc123")
	dispatch.AppendString(2, "rt_token456")
	dispatch.AppendBool(3, true)

	wakeup := NewBuilder()
	wakeup.AppendMessage(1, dispatch.Bytes())
	wakeup.AppendString(4, "dev-uuid-1")

	ev, err := DecodeWakeUpPayload(2, "remote-tool-wakeup/1", wakeup.Bytes())
	if err != nil {
		t.Fatalf("WakeUp 디코딩 실패: %v", err)
	}
	if ev.SessionRoutingKey != "rk_abc123" || ev.RoutingToken != "rt_token456" || ev.TargetDeviceID != "dev-uuid-1" {
		t.Fatalf("WakeUp 필드 불일치: %+v", ev)
	}
}

func TestDecodeMultiWatchResponseBatch(t *testing.T) {
	part1 := NewBuilder()
	part1.AppendString(1, "remote-tool-wakeup")
	part2 := NewBuilder()
	part2.AppendString(1, "evt-1")

	objPath := NewBuilder()
	objPath.AppendMessage(1, part1.Bytes())
	objPath.AppendMessage(1, part2.Bytes())

	objChange := NewBuilder()
	objChange.AppendInt64(1, 42)
	objChange.AppendBytes(2, []byte("payload-bytes"))
	objChange.AppendMessage(6, objPath.Bytes())

	changeBatch := NewBuilder()
	changeBatch.AppendMessage(1, objChange.Bytes())

	watchResp := NewBuilder()
	watchResp.AppendMessage(2, changeBatch.Bytes())

	multiResp := NewBuilder()
	multiResp.AppendInt64Always(1, 2)
	multiResp.AppendMessage(2, watchResp.Bytes())

	batch := NewBuilder()
	batch.AppendMessage(1, multiResp.Bytes())

	changes, needsReconnect, err := DecodeMultiWatchResponseBatch(batch.Bytes())
	if err != nil || needsReconnect {
		t.Fatalf("DecodeMultiWatchResponseBatch 실패: %v (reconnect=%v)", err, needsReconnect)
	}
	if len(changes) != 1 {
		t.Fatalf("ObjectChange 개수 불일치: %d", len(changes))
	}
	if changes[0].StreamID != 2 || changes[0].ObjectPath != "remote-tool-wakeup/evt-1" || string(changes[0].Payload) != "payload-bytes" {
		t.Fatalf("ObjectChange 필드 불일치: %+v", changes[0])
	}
}
