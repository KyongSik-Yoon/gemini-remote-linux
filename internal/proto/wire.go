package proto

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Protobuf Wire Types
const (
	WireVarint  = 0
	WireFixed64 = 1
	WireBytes   = 2
	WireFixed32 = 5
)

// Builder는 수동 Protobuf 직렬화를 위한 경량 버퍼다.
type Builder struct {
	buf []byte
}

func NewBuilder() *Builder {
	return &Builder{}
}

func (b *Builder) Bytes() []byte {
	return b.buf
}

func (b *Builder) AppendTag(fieldNum uint32, wireType uint8) {
	b.AppendVarint((uint64(fieldNum) << 3) | uint64(wireType))
}

func (b *Builder) AppendVarint(v uint64) {
	var tmp [10]byte
	n := binary.PutUvarint(tmp[:], v)
	b.buf = append(b.buf, tmp[:n]...)
}

func (b *Builder) AppendInt32(fieldNum uint32, v int32) {
	if v == 0 {
		return
	}
	b.AppendTag(fieldNum, WireVarint)
	b.AppendVarint(uint64(uint32(v)))
}

func (b *Builder) AppendInt32Always(fieldNum uint32, v int32) {
	b.AppendTag(fieldNum, WireVarint)
	b.AppendVarint(uint64(uint32(v)))
}

func (b *Builder) AppendInt64(fieldNum uint32, v int64) {
	if v == 0 {
		return
	}
	b.AppendTag(fieldNum, WireVarint)
	b.AppendVarint(uint64(v))
}

func (b *Builder) AppendInt64Always(fieldNum uint32, v int64) {
	b.AppendTag(fieldNum, WireVarint)
	b.AppendVarint(uint64(v))
}

func (b *Builder) AppendBool(fieldNum uint32, v bool) {
	if !v {
		return
	}
	b.AppendTag(fieldNum, WireVarint)
	b.AppendVarint(1)
}

func (b *Builder) AppendBoolAlways(fieldNum uint32, v bool) {
	b.AppendTag(fieldNum, WireVarint)
	if v {
		b.AppendVarint(1)
	} else {
		b.AppendVarint(0)
	}
}

func (b *Builder) AppendFixed64(fieldNum uint32, v uint64) {
	b.AppendTag(fieldNum, WireFixed64)
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], v)
	b.buf = append(b.buf, tmp[:]...)
}

func (b *Builder) AppendFloat64(fieldNum uint32, v float64) {
	b.AppendFixed64(fieldNum, math.Float64bits(v))
}

func (b *Builder) AppendString(fieldNum uint32, s string) {
	if s == "" {
		return
	}
	b.AppendBytes(fieldNum, []byte(s))
}

func (b *Builder) AppendStringAlways(fieldNum uint32, s string) {
	b.AppendTag(fieldNum, WireBytes)
	b.AppendVarint(uint64(len(s)))
	b.buf = append(b.buf, s...)
}

func (b *Builder) AppendBytes(fieldNum uint32, data []byte) {
	if len(data) == 0 {
		return
	}
	b.AppendTag(fieldNum, WireBytes)
	b.AppendVarint(uint64(len(data)))
	b.buf = append(b.buf, data...)
}

func (b *Builder) AppendMessage(fieldNum uint32, msg []byte) {
	b.AppendTag(fieldNum, WireBytes)
	b.AppendVarint(uint64(len(msg)))
	b.buf = append(b.buf, msg...)
}

// Field는 디코딩된 단일 Protobuf 필드를 나타낸다.
type Field struct {
	Num      uint32
	WireType uint8
	Varint   uint64
	Fixed64  uint64
	Fixed32  uint32
	Bytes    []byte
}

func (f Field) String() string {
	return string(f.Bytes)
}

// ParseFields는 Protobuf 바이트 슬라이스를 필드 목록으로 파싱한다.
func ParseFields(data []byte) ([]Field, error) {
	var out []Field
	i := 0
	for i < len(data) {
		tag, n := binary.Uvarint(data[i:])
		if n <= 0 {
			return out, fmt.Errorf("잘못된 protobuf tag varint (offset=%d)", i)
		}
		i += n
		fieldNum := uint32(tag >> 3)
		wireType := uint8(tag & 0x07)

		switch wireType {
		case WireVarint:
			val, vn := binary.Uvarint(data[i:])
			if vn <= 0 {
				return out, fmt.Errorf("잘못된 varint 값 (field=%d, offset=%d)", fieldNum, i)
			}
			i += vn
			out = append(out, Field{Num: fieldNum, WireType: wireType, Varint: val})
		case WireFixed64:
			if i+8 > len(data) {
				return out, fmt.Errorf("잘못된 fixed64 길이 (field=%d)", fieldNum)
			}
			val := binary.LittleEndian.Uint64(data[i : i+8])
			i += 8
			out = append(out, Field{Num: fieldNum, WireType: wireType, Fixed64: val})
		case WireBytes:
			length, ln := binary.Uvarint(data[i:])
			if ln <= 0 || i+ln+int(length) > len(data) {
				return out, fmt.Errorf("잘못된 bytes 길이 (field=%d, len=%d)", fieldNum, length)
			}
			i += ln
			b := data[i : i+int(length)]
			i += int(length)
			out = append(out, Field{Num: fieldNum, WireType: wireType, Bytes: b})
		case WireFixed32:
			if i+4 > len(data) {
				return out, fmt.Errorf("잘못된 fixed32 길이 (field=%d)", fieldNum)
			}
			val := binary.LittleEndian.Uint32(data[i : i+4])
			i += 4
			out = append(out, Field{Num: fieldNum, WireType: wireType, Fixed32: val})
		default:
			return out, fmt.Errorf("지원하지 않는 wireType=%d (field=%d)", wireType, fieldNum)
		}
	}
	return out, nil
}

// EncodeTimestamp는 google.protobuf.Timestamp 메시지를 인코딩한다.
func EncodeTimestamp(t time.Time) []byte {
	b := NewBuilder()
	b.AppendInt64(1, t.Unix())
	b.AppendInt32(2, int32(t.Nanosecond()))
	return b.Bytes()
}

// EncodeStruct는 Go map[string]any를 google.protobuf.Struct로 직렬화한다.
func EncodeStruct(m map[string]any) []byte {
	b := NewBuilder()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		entry := NewBuilder()
		entry.AppendStringAlways(1, k)
		entry.AppendMessage(2, EncodeValue(m[k]))
		b.AppendMessage(1, entry.Bytes())
	}
	return b.Bytes()
}

// EncodeValue는 임의의 Go 값을 google.protobuf.Value로 직렬화한다.
func EncodeValue(v any) []byte {
	b := NewBuilder()
	if v == nil {
		b.AppendInt32Always(1, 0) // null_value = 0
		return b.Bytes()
	}
	switch val := v.(type) {
	case bool:
		b.AppendBoolAlways(4, val)
	case int:
		b.AppendFloat64(2, float64(val))
	case int32:
		b.AppendFloat64(2, float64(val))
	case int64:
		b.AppendFloat64(2, float64(val))
	case float64:
		b.AppendFloat64(2, val)
	case float32:
		b.AppendFloat64(2, float64(val))
	case string:
		b.AppendStringAlways(3, val)
	case map[string]any:
		b.AppendMessage(5, EncodeStruct(val))
	case []any:
		list := NewBuilder()
		for _, item := range val {
			list.AppendMessage(1, EncodeValue(item))
		}
		b.AppendMessage(6, list.Bytes())
	case []string:
		list := NewBuilder()
		for _, item := range val {
			list.AppendMessage(1, EncodeValue(item))
		}
		b.AppendMessage(6, list.Bytes())
	case []map[string]any:
		list := NewBuilder()
		for _, item := range val {
			list.AppendMessage(1, EncodeValue(item))
		}
		b.AppendMessage(6, list.Bytes())
	default:
		b.AppendStringAlways(3, fmt.Sprintf("%v", val))
	}
	return b.Bytes()
}

// DecodeStruct는 google.protobuf.Struct 바이트를 map[string]any로 디코딩한다.
func DecodeStruct(data []byte) map[string]any {
	out := make(map[string]any)
	fields, err := ParseFields(data)
	if err != nil {
		return out
	}
	for _, f := range fields {
		if f.Num == 1 && f.WireType == WireBytes {
			entryFields, err := ParseFields(f.Bytes)
			if err != nil {
				continue
			}
			var key string
			var val any
			for _, ef := range entryFields {
				if ef.Num == 1 && ef.WireType == WireBytes {
					key = string(ef.Bytes)
				} else if ef.Num == 2 && ef.WireType == WireBytes {
					val = DecodeValue(ef.Bytes)
				}
			}
			if key != "" {
				out[key] = val
			}
		}
	}
	return out
}

// DecodeValue는 google.protobuf.Value 바이트를 Go 값으로 디코딩한다.
func DecodeValue(data []byte) any {
	fields, err := ParseFields(data)
	if err != nil {
		return nil
	}
	for _, f := range fields {
		switch f.Num {
		case 1: // null_value
			return nil
		case 2: // number_value (double)
			return math.Float64frombits(f.Fixed64)
		case 3: // string_value
			return string(f.Bytes)
		case 4: // bool_value
			return f.Varint != 0
		case 5: // struct_value
			return DecodeStruct(f.Bytes)
		case 6: // list_value
			var list []any
			lf, _ := ParseFields(f.Bytes)
			for _, item := range lf {
				if item.Num == 1 && item.WireType == WireBytes {
					list = append(list, DecodeValue(item.Bytes))
				}
			}
			return list
		}
	}
	return nil
}

// SchemaType enum (Assistant_Lamda_V2_FunctionCalling_Schema.TypeEnum @ 0xa0eeb00)
const (
	TypeUnspecified int32 = 0
	TypeString      int32 = 1
	TypeInteger     int32 = 2
	TypeNumber      int32 = 3
	TypeBoolean     int32 = 4
	TypeArray       int32 = 5
	TypeObject      int32 = 6
)

// SchemaDef는 Assistant_Lamda_V2_FunctionCalling_Schema를 표현한다.
type SchemaDef struct {
	Type        int32
	Description string
	Nullable    bool
	Enum        []string
	Items       *SchemaDef
	Properties  map[string]*SchemaDef
	Required    []string
}

// EncodeSchema는 Assistant_Lamda_V2_FunctionCalling_Schema 필드 번호(1~8)에 맞춰 직렬화한다.
func EncodeSchema(s *SchemaDef) []byte {
	if s == nil {
		return nil
	}
	b := NewBuilder()
	b.AppendInt32(1, s.Type)
	b.AppendString(2, s.Description)
	b.AppendBool(3, s.Nullable)
	for _, e := range s.Enum {
		b.AppendString(4, e)
	}
	if s.Items != nil {
		b.AppendMessage(5, EncodeSchema(s.Items))
	}
	if len(s.Properties) > 0 {
		keys := make([]string, 0, len(s.Properties))
		for k := range s.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			entry := NewBuilder()
			entry.AppendStringAlways(1, k)
			entry.AppendMessage(2, EncodeSchema(s.Properties[k]))
			b.AppendMessage(6, entry.Bytes())
		}
	}
	for _, r := range s.Required {
		b.AppendString(8, r)
	}
	return b.Bytes()
}

// FunctionDecl은 Assistant_Lamda_V2_FunctionCalling_FunctionDeclaration을 표현한다.
type FunctionDecl struct {
	Name        string
	Description string
	Parameters  *SchemaDef
}

func EncodeFunctionDecl(fn FunctionDecl) []byte {
	b := NewBuilder()
	b.AppendString(1, fn.Name)
	b.AppendString(2, fn.Description)
	if fn.Parameters != nil {
		b.AppendMessage(3, EncodeSchema(fn.Parameters))
	}
	return b.Bytes()
}

// EncodeRequestMetadata는 RobinNotifyService/RobinService 호환 RequestMetadata(0xa017e90)를 생성한다.
func EncodeRequestMetadata(locale, timeZone, appVersion, sessionID, deviceID string) []byte {
	logMeta := NewBuilder()
	if deviceID != "" {
		logMeta.AppendString(17, strings.ToUpper(deviceID)) // device_id
	}

	b := NewBuilder()
	b.AppendMessage(3, logMeta.Bytes()) // logging_metadata
	return b.Bytes()
}

// BootstrapFileInfo는 DesktopDeviceInfo.BootstrapFile 구조체다.
type BootstrapFileInfo struct {
	Filename         string
	LastModifiedTime time.Time
	Content          string
}

// BundledMcpToolInfo는 DesktopDeviceInfo.BundledMcpServer.BundledMcpTool 구조체다.
type BundledMcpToolInfo struct {
	Name       string
	SchemaHash string
}

// BundledMcpServerInfo는 DesktopDeviceInfo.BundledMcpServer 구조체다.
type BundledMcpServerInfo struct {
	ServerID string
	Enabled  bool
	Tools    []BundledMcpToolInfo
}

// DesktopDeviceParams는 Assistant_Proactive_DesktopDeviceInfo(0xa011900) 생성 파라미터다.
type DesktopDeviceParams struct {
	DeviceID         string
	DeviceName       string
	ToolDeclarations []FunctionDecl
	ConnectedFolders []string
	BootstrapFiles   []BootstrapFileInfo
	BundledMcp       []BundledMcpServerInfo
}

// EncodeDesktopDeviceInfo는 Assistant_Proactive_DesktopDeviceInfo 메시지를 인코딩한다.
func EncodeDesktopDeviceInfo(p DesktopDeviceParams) []byte {
	b := NewBuilder()
	b.AppendString(1, p.DeviceName)
	for _, decl := range p.ToolDeclarations {
		b.AppendMessage(2, EncodeFunctionDecl(decl))
	}
	for _, folder := range p.ConnectedFolders {
		fb := NewBuilder()
		fb.AppendString(1, folder)
		b.AppendMessage(3, fb.Bytes())
	}
	for _, bf := range p.BootstrapFiles {
		bb := NewBuilder()
		bb.AppendString(1, bf.Filename)
		if !bf.LastModifiedTime.IsZero() {
			bb.AppendMessage(2, EncodeTimestamp(bf.LastModifiedTime))
		}
		bb.AppendString(3, bf.Content)
		b.AppendMessage(4, bb.Bytes())
	}
	b.AppendMessage(5, EncodeTimestamp(time.Now().UTC()))
	b.AppendString(6, p.DeviceID)

	// 7: supported_client_capabilities (macOS Gemini.app 실측 Capability ID 목록)
	for _, capID := range []int32{379, 467, 451, 506, 524, 453} {
		b.AppendInt32(7, capID)
	}
	b.AppendBool(8, true) // enable_remote_command = true

	// 9: consent { 1: find_folders_consented = true, 2: network_access_policy = 3 (ALWAYS) }
	consent := NewBuilder()
	consent.AppendBool(1, true)
	consent.AppendInt32(2, 3)
	b.AppendMessage(9, consent.Bytes())

	// 10: bundled_mcp_servers { 1: server_id, 2: enabled (bool) }
	for _, srv := range p.BundledMcp {
		sb := NewBuilder()
		sb.AppendString(1, srv.ServerID)
		sb.AppendBool(2, srv.Enabled)
		b.AppendMessage(10, sb.Bytes())
	}
	return b.Bytes()
}

// EncodeRegisterDeviceRequest는 Assistant_Mobile_Robin_Notify_RegisterDeviceRequest(0x9835b80)를 인코딩한다.
func EncodeRegisterDeviceRequest(reqMeta []byte, appVersion string, desktopInfo []byte) []byte {
	devInfo := NewBuilder()
	devInfo.AppendString(1, appVersion)

	b := NewBuilder()
	if len(reqMeta) > 0 {
		b.AppendMessage(1, reqMeta)
	}
	b.AppendMessage(4, devInfo.Bytes())
	b.AppendMessage(5, desktopInfo)
	b.AppendInt32(6, 9) // target_surface = GEMINI_MACOS (9)
	return b.Bytes()
}

// RegisterDeviceResult는 RegisterDeviceResponse 디코딩 결과다.
type RegisterDeviceResult struct {
	FCMToken        string
	TargetSurface   int32
	DeviceID        string
	DeliveryAddress string
	AppVersion      string
}

// DecodeRegisterDeviceResponse는 RegisterDeviceResponse(0x9835b80)를 파싱한다.
func DecodeRegisterDeviceResponse(data []byte) (*RegisterDeviceResult, error) {
	fields, err := ParseFields(data)
	if err != nil {
		return nil, err
	}
	res := &RegisterDeviceResult{}
	for _, f := range fields {
		switch f.Num {
		case 2:
			res.FCMToken = string(f.Bytes)
		case 3: // device_registration
			regFields, _ := ParseFields(f.Bytes)
			for _, rf := range regFields {
				switch rf.Num {
				case 1:
					res.TargetSurface = int32(rf.Varint)
				case 2:
					res.DeviceID = string(rf.Bytes)
				case 3:
					res.DeliveryAddress = string(rf.Bytes)
				case 4:
					res.AppVersion = string(rf.Bytes)
				}
			}
		}
	}
	return res, nil
}

// EncodeUnregisterDeviceRequest는 Assistant_Mobile_Robin_Notify_UnregisterDeviceRequest를 인코딩한다.
func EncodeUnregisterDeviceRequest(reqMeta []byte, deviceID string) []byte {
	b := NewBuilder()
	if len(reqMeta) > 0 {
		b.AppendMessage(1, reqMeta)
	}
	b.AppendInt32(4, 9) // target_surface = GEMINI_MACOS (9)
	b.AppendString(5, deviceID)
	return b.Bytes()
}

// EncodeListDevicesRequest는 Assistant_Mobile_Robin_Notify_ListDevicesRequest(0x982e450)를 인코딩한다.
func EncodeListDevicesRequest(reqMeta []byte) []byte {
	b := NewBuilder()
	if len(reqMeta) > 0 {
		b.AppendMessage(1, reqMeta)
	}
	b.AppendInt32(4, 9) // target_surfaces = [GEMINI_MACOS (9)]
	return b.Bytes()
}

// RegisteredDeviceSummary는 ListDevicesResponse의 개별 기기 요약이다.
type RegisteredDeviceSummary struct {
	DeliveryToken string
	DeviceID      string
	DeviceName    string
	RemoteEnabled bool
	ToolCount     int
	Folders       []string
}

// DecodeListDevicesResponse는 ListDevicesResponse(0x982e450)를 파싱한다.
func DecodeListDevicesResponse(data []byte) ([]RegisteredDeviceSummary, error) {
	fields, err := ParseFields(data)
	if err != nil {
		return nil, err
	}
	var out []RegisteredDeviceSummary
	for _, f := range fields {
		if f.Num == 1 && f.WireType == WireBytes {
			dev := RegisteredDeviceSummary{}
			df, _ := ParseFields(f.Bytes)
			for _, d := range df {
				switch d.Num {
				case 1:
					dev.DeliveryToken = string(d.Bytes)
				case 2: // desktop_device_info
					infoFields, _ := ParseFields(d.Bytes)
					for _, inf := range infoFields {
						switch inf.Num {
						case 1:
							dev.DeviceName = string(inf.Bytes)
						case 2:
							dev.ToolCount++
						case 3:
							ff, _ := ParseFields(inf.Bytes)
							for _, folderField := range ff {
								if folderField.Num == 1 {
									dev.Folders = append(dev.Folders, string(folderField.Bytes))
								}
							}
						case 6:
							dev.DeviceID = string(inf.Bytes)
						case 8:
							dev.RemoteEnabled = inf.Varint != 0
						}
					}
				}
			}
			out = append(out, dev)
		}
	}
	return out, nil
}

// EncodeReportClientPresenceRequest는 ReportClientPresenceRequest(0x9e26340)를 인코딩한다.
// Field 1: string focused_conversation_id, Field 2: bool is_visible, Field 3: RequestMetadata request_metadata
func EncodeReportClientPresenceRequest(reqMeta []byte, _ string) []byte {
	b := NewBuilder()
	b.AppendBool(2, true) // is_visible = true
	if len(reqMeta) > 0 {
		b.AppendMessage(3, reqMeta)
	}
	return b.Bytes()
}

// EncodeRemoteToolConnectRequest는 ExecuteRemoteToolsRequest(connect) 메시지를 인코딩한다.
func EncodeRemoteToolConnectRequest(reqMeta []byte, sessionRoutingKey, deviceID, routingToken string) []byte {
	conn := NewBuilder()
	conn.AppendString(1, sessionRoutingKey)
	conn.AppendString(2, strings.ToUpper(deviceID))
	conn.AppendString(3, routingToken)

	b := NewBuilder()
	b.AppendMessage(1, conn.Bytes())
	if len(reqMeta) > 0 {
		b.AppendMessage(4, reqMeta)
	}
	return b.Bytes()
}

// AttachmentInput은 FunctionResponse.attachments(UserQueryAttachment)에 들어갈 첨부 정보다.
type AttachmentInput struct {
	BlobID     string
	FileType   int32
	MimeType   string
	FileSource int32
	Name       string
}

// FunctionResponseInput은 ClientFunctionCallResult.FunctionResponse 입력이다.
type FunctionResponseInput struct {
	Name        string
	ID          string
	Response    map[string]any
	Attachments []AttachmentInput
}

// EncodeRemoteToolResultRequest는 ExecuteRemoteToolsRequest(tool_result) 메시지를 인코딩한다.
func EncodeRemoteToolResultRequest(reqMeta []byte, responses []FunctionResponseInput) []byte {
	resBuilder := NewBuilder()
	for _, r := range responses {
		fr := NewBuilder()
		fr.AppendString(1, r.Name)
		fr.AppendString(2, r.ID)
		if r.Response != nil {
			fr.AppendMessage(3, EncodeStruct(r.Response))
		}
		for _, att := range r.Attachments {
			blob := NewBuilder()
			blob.AppendString(1, att.BlobID)
			blob.AppendInt32(2, att.FileType)
			blob.AppendString(3, att.MimeType)
			blob.AppendInt32(4, att.FileSource)

			uqa := NewBuilder()
			uqa.AppendMessage(1, blob.Bytes())
			uqa.AppendString(2, att.Name)
			fr.AppendMessage(4, uqa.Bytes())
		}
		resBuilder.AppendMessage(1, fr.Bytes())
	}

	b := NewBuilder()
	b.AppendMessage(2, resBuilder.Bytes())
	if len(reqMeta) > 0 {
		b.AppendMessage(4, reqMeta)
	}
	return b.Bytes()
}

// EncodeRemoteToolFailureRequest는 ExecuteRemoteToolsRequest(tool_failure) 메시지를 인코딩한다.
func EncodeRemoteToolFailureRequest(reqMeta []byte, callID, errMsg string, errCode int32) []byte {
	fail := NewBuilder()
	fail.AppendString(1, callID)
	fail.AppendString(2, errMsg)
	fail.AppendInt32(3, errCode)

	b := NewBuilder()
	b.AppendMessage(3, fail.Bytes())
	if len(reqMeta) > 0 {
		b.AppendMessage(4, reqMeta)
	}
	return b.Bytes()
}

// RemoteFunctionCall은 서버가 내려보낸 단일 도구 호출이다.
type RemoteFunctionCall struct {
	Name string
	ID   string
	Args map[string]any
}

// RemoteToolServerEvent는 ExecuteRemoteToolsResponse 디코딩 결과다.
type RemoteToolServerEvent struct {
	ConnectAck      bool
	Connected       bool
	SessionID       string
	FunctionCalls   []RemoteFunctionCall
	SessionComplete bool
	ConversationID  string
}

// DecodeExecuteRemoteToolsResponse는 ExecuteRemoteToolsResponse(0x9e46950)를 파싱한다.
func DecodeExecuteRemoteToolsResponse(data []byte) (*RemoteToolServerEvent, error) {
	fields, err := ParseFields(data)
	if err != nil {
		return nil, err
	}
	ev := &RemoteToolServerEvent{}
	for _, f := range fields {
		switch f.Num {
		case 1: // connect_ack (RemoteToolConnectResponse)
			ev.ConnectAck = true
			cf, _ := ParseFields(f.Bytes)
			for _, c := range cf {
				if c.Num == 1 && c.WireType == WireVarint {
					ev.Connected = c.Varint != 0
				}
				if c.Num == 2 && c.WireType == WireBytes {
					ev.SessionID = string(c.Bytes)
				}
			}
		case 2: // tool_call (ClientFunctionCallArgs)
			tf, _ := ParseFields(f.Bytes)
			for _, t := range tf {
				if t.Num == 1 && t.WireType == WireBytes {
					call := RemoteFunctionCall{Args: make(map[string]any)}
					callFields, _ := ParseFields(t.Bytes)
					for _, cf := range callFields {
						switch cf.Num {
						case 1:
							call.Name = string(cf.Bytes)
						case 2:
							call.ID = string(cf.Bytes)
						case 3:
							call.Args = DecodeStruct(cf.Bytes)
						}
					}
					ev.FunctionCalls = append(ev.FunctionCalls, call)
				}
			}
		case 3: // session_complete (RemoteToolSessionComplete)
			ev.SessionComplete = true
		case 5: // conversation_id
			ev.ConversationID = string(f.Bytes)
		}
	}
	return ev, nil
}

// WakeUpEvent는 Punctual 채널 또는 FCM으로 수신한 원격 도구 실행 트리거 정보다.
type WakeUpEvent struct {
	SessionRoutingKey string
	RoutingToken      string
	TargetDeviceID    string
	IsLocalExecution  bool
	ScheduledTaskName string
	IsClientSyncHint  bool
}

func extractConversationKey(s string) string {
	idx := strings.Index(s, "c_")
	if idx < 0 {
		return ""
	}
	sub := s[idx:]
	if slash := strings.IndexByte(sub, '/'); slash > 0 {
		sub = sub[:slash]
	}
	if len(sub) > 2 {
		return sub
	}
	return ""
}

// DecodeWakeUpPayload는 ClientSyncSignal 또는 RemoteWakeUpPayload를 모두 시도해 파싱한다.
func DecodeWakeUpPayload(streamID int64, objectPath string, payload []byte) (*WakeUpEvent, error) {
	fields, err := ParseFields(payload)
	if err != nil {
		return nil, err
	}
	ev := &WakeUpEvent{}

	parseRemoteWakeUp := func() bool {
		for _, f := range fields {
			switch f.Num {
			case 1: // tool_dispatch (RemoteToolDispatch)
				if f.WireType == WireBytes {
					subFields, _ := ParseFields(f.Bytes)
					for _, sf := range subFields {
						switch sf.Num {
						case 1:
							if sf.WireType == WireBytes {
								ev.SessionRoutingKey = string(sf.Bytes)
							}
						case 2:
							if sf.WireType == WireBytes {
								ev.RoutingToken = string(sf.Bytes)
							}
						case 3:
							if sf.WireType == WireVarint {
								ev.IsLocalExecution = sf.Varint != 0
							}
						}
					}
				}
			case 2: // scheduled_task (ScheduledTaskTrigger)
				if f.WireType == WireBytes {
					sf, _ := ParseFields(f.Bytes)
					for _, s := range sf {
						if s.Num == 1 && s.WireType == WireBytes {
							ev.ScheduledTaskName = string(s.Bytes)
						}
					}
				}
			case 4: // target_device_id
				if f.WireType == WireBytes {
					ev.TargetDeviceID = string(f.Bytes)
				}
			}
		}
		return ev.SessionRoutingKey != "" || ev.ScheduledTaskName != ""
	}

	if strings.Contains(objectPath, "remote-tool-wakeup") {
		if parseRemoteWakeUp() {
			return ev, nil
		}
		return nil, fmt.Errorf("유효한 RemoteWakeUpPayload 필드 없음 (stream=%d, path=%s)", streamID, objectPath)
	}

	// ClientSyncSignal 파싱 시도
	var actionNums []uint32
	hintConvKey := extractConversationKey(objectPath)
	for _, f := range fields {
		if f.Num == 1 && f.WireType == WireBytes { // repeated ClientAction
			subFields, _ := ParseFields(f.Bytes)
			for _, sf := range subFields {
				actionNums = append(actionNums, sf.Num)
				switch sf.Num {
				case 1, 21: // RefreshConversation (1), RefreshConversationsMetadata (21)
					if sf.WireType == WireBytes && hintConvKey == "" {
						af, _ := ParseFields(sf.Bytes)
						for _, a := range af {
							if a.Num == 1 && a.WireType == WireBytes {
								if k := extractConversationKey(string(a.Bytes)); k != "" {
									hintConvKey = k
								}
							}
						}
					}
				case 25: // WakeUpForRemoteTool
					if sf.WireType == WireBytes {
						wf, _ := ParseFields(sf.Bytes)
						for _, w := range wf {
							switch w.Num {
							case 1:
								ev.SessionRoutingKey = string(w.Bytes)
							case 2:
								ev.RoutingToken = string(w.Bytes)
							case 4:
								ev.TargetDeviceID = string(w.Bytes)
							}
						}
					}
				case 27: // RefreshDeviceStatus
					if sf.WireType == WireBytes {
						df, _ := ParseFields(sf.Bytes)
						var devID string
						var enabled bool
						for _, d := range df {
							if d.Num == 1 && d.WireType == WireBytes {
								devID = string(d.Bytes)
							} else if d.Num == 3 && d.WireType == WireVarint {
								enabled = d.Varint != 0
							}
						}
						return nil, fmt.Errorf("ClientSync 기기 상태 갱신 수신 (device_id=%s, remote_enabled=%v)", devID, enabled)
					}
				}
			}
		}
	}
	if ev.SessionRoutingKey != "" {
		return ev, nil
	}
	if hintConvKey != "" {
		return &WakeUpEvent{
			SessionRoutingKey: hintConvKey,
			IsClientSyncHint:  true,
		}, nil
	}
	if len(actionNums) > 0 {
		return nil, fmt.Errorf("일반 ClientSync 액션 수신 (stream=%d, path=%s, actions=%v)", streamID, objectPath, actionNums)
	}
	return nil, fmt.Errorf("유효한 WakeUp 필드 없음 (stream=%d, path=%s)", streamID, objectPath)
}

// buildPunctualHeader는 Punctual Header { 1: version=9, 2: library=10 (XPLAT) }를 생성한다.
func buildPunctualHeader() []byte {
	hdr := NewBuilder()
	hdr.AppendInt32(1, 9)
	hdr.AppendInt32(2, 10)
	return hdr.Bytes()
}

// buildPunctualPointWatchRequest는 단일 ObjectId(point) 대상 WatchRequest를 생성한다.
func buildPunctualPointWatchRequest(project, partStr string) []byte {
	proj := NewBuilder()
	proj.AppendString(1, project)

	owner := NewBuilder()
	owner.AppendBool(2, true) // is_private_to_user = true

	part := NewBuilder()
	part.AppendString(1, partStr)

	objPath := NewBuilder()
	objPath.AppendMessage(1, part.Bytes())

	objID := NewBuilder()
	objID.AppendMessage(1, proj.Bytes())
	objID.AppendMessage(2, owner.Bytes())
	objID.AppendMessage(3, objPath.Bytes())

	watchReq := NewBuilder()
	watchReq.AppendMessage(2, objID.Bytes())
	watchReq.AppendMessage(4, buildPunctualHeader())
	watchReq.AppendBool(8, true)  // start_time_chosen_by_punctual = true
	watchReq.AppendInt32(9, 1)    // stream_initiation_reason = INITIAL_WATCH
	return watchReq.Bytes()
}

// buildPunctualPrefixWatchRequest는 ObjectIdPrefix(prefix) 대상 WatchRequest를 생성한다.
func buildPunctualPrefixWatchRequest(project, partStr string) []byte {
	proj := NewBuilder()
	proj.AppendString(1, project)

	owner := NewBuilder()
	owner.AppendBool(2, true) // is_private_to_user = true

	part := NewBuilder()
	part.AppendString(1, partStr)

	objPath := NewBuilder()
	objPath.AppendMessage(1, part.Bytes())

	prefix := NewBuilder()
	prefix.AppendMessage(1, proj.Bytes())
	prefix.AppendMessage(2, owner.Bytes())
	prefix.AppendMessage(3, objPath.Bytes())

	watchReq := NewBuilder()
	watchReq.AppendMessage(4, buildPunctualHeader())
	watchReq.AppendMessage(6, prefix.Bytes())
	watchReq.AppendBool(8, true)  // start_time_chosen_by_punctual = true
	watchReq.AppendInt32(9, 1)    // stream_initiation_reason = INITIAL_WATCH
	return watchReq.Bytes()
}

// EncodeChooseServerRequest는 Signaler Punctual ChooseServerRequest(field 1: WatchRequest)를 인코딩한다.
func EncodeChooseServerRequest(project, path string) []byte {
	b := NewBuilder()
	b.AppendMessage(1, buildPunctualPointWatchRequest(project, path))
	return b.Bytes()
}

// DecodeChooseServerResponse는 ChooseServerResponse에서 gsessionid(field 1)를 추출한다.
func DecodeChooseServerResponse(data []byte) (string, error) {
	fields, err := ParseFields(data)
	if err != nil {
		return "", err
	}
	for _, f := range fields {
		if f.Num == 1 && f.WireType == WireBytes {
			return string(f.Bytes), nil
		}
	}
	return "", fmt.Errorf("ChooseServerResponse에 gsessionid 없음")
}

// EncodeMultiWatchRequestBatch는 Punctual `/punctual/multi-watch/channel` 구독 요청 배치를 생성한다.
func EncodeMultiWatchRequestBatch(project string) []byte {
	streams := []struct {
		id  int64
		req []byte
	}{
		{1, buildPunctualPointWatchRequest("assistant-bard", "bard-client-sync")},
		{2, buildPunctualPrefixWatchRequest("assistant-bard", "remote-tool-wakeup")},
		{5, buildPunctualPrefixWatchRequest("assistant-bard", "bard-client-sync")},
		{7, buildPunctualPointWatchRequest("assistant-bard", "remote-tool-wakeup")},
	}

	batch := NewBuilder()
	for _, s := range streams {
		req := NewBuilder()
		req.AppendInt64Always(1, s.id)
		req.AppendMessage(2, s.req)
		req.AppendInt32(4, 3) // affinitization = 3
		batch.AppendMessage(1, req.Bytes())
	}
	return batch.Bytes()
}

// PunctualObjectChange는 MultiWatchResponseBatch 내부의 개별 ObjectChange다.
type PunctualObjectChange struct {
	StreamID   int64
	ObjectPath string
	Payload    []byte
}

func parseObjectPath(data []byte) string {
	fields, err := ParseFields(data)
	if err != nil {
		return ""
	}
	var parts []string
	for _, f := range fields {
		if f.Num == 1 && f.WireType == WireBytes { // Part { 1: string }
			pf, perr := ParseFields(f.Bytes)
			if perr == nil {
				for _, p := range pf {
					if p.Num == 1 && p.WireType == WireBytes {
						parts = append(parts, string(p.Bytes))
					}
				}
			}
		} else if (f.Num == 3 || f.Num == 4) && f.WireType == WireBytes {
			parts = append(parts, string(f.Bytes))
		}
	}
	return strings.Join(parts, "/")
}

func parseObjectChange(streamID int64, data []byte) (PunctualObjectChange, bool) {
	change := PunctualObjectChange{StreamID: streamID}
	cf, err := ParseFields(data)
	if err != nil {
		return change, false
	}
	for _, c := range cf {
		switch c.Num {
		case 2: // bytes payload
			if c.WireType == WireBytes {
				change.Payload = c.Bytes
			}
		case 6: // ObjectPath object_path
			if c.WireType == WireBytes {
				change.ObjectPath = parseObjectPath(c.Bytes)
			}
		}
	}
	return change, len(change.Payload) > 0
}

// DecodeMultiWatchResponseBatch는 MultiWatchResponseBatch에서 모든 ObjectChange와 재연결 필요 여부를 추출한다.
func DecodeMultiWatchResponseBatch(data []byte) ([]PunctualObjectChange, bool, error) {
	fields, err := ParseFields(data)
	if err != nil {
		return nil, false, err
	}
	var out []PunctualObjectChange
	needsReconnect := false
	for _, f := range fields {
		if f.Num == 1 && f.WireType == WireBytes { // MultiWatchResponse
			var streamID int64
			rf, _ := ParseFields(f.Bytes)
			for _, r := range rf {
				if r.Num == 1 && r.WireType == WireVarint {
					streamID = int64(r.Varint)
				} else if r.Num == 2 && r.WireType == WireBytes { // WatchResponse
					wf, _ := ParseFields(r.Bytes)
					for _, w := range wf {
						switch w.Num {
						case 1: // WatchStart
							fmt.Printf("[punctual] 스트림 시작 확인 (stream=%d)\n", streamID)
						case 2: // ChangeBatch { 1: repeated ObjectChange }
							if w.WireType == WireBytes {
								cbf, _ := ParseFields(w.Bytes)
								matched := false
								for _, cb := range cbf {
									if cb.Num == 1 && cb.WireType == WireBytes {
										if ch, ok := parseObjectChange(streamID, cb.Bytes); ok {
											out = append(out, ch)
											matched = true
										}
									}
								}
								if !matched {
									if ch, ok := parseObjectChange(streamID, w.Bytes); ok {
										out = append(out, ch)
									}
								}
							}
						case 3: // Checkpoint / Watermark (주기적 동기화 지점)
							// 정상적인 주기적 체크포인트이므로 세션을 유지한다.
						case 4, 5: // error (4) or done (5) -> com.google.rpc.Status
							if w.WireType == WireBytes {
								sf, _ := ParseFields(w.Bytes)
								var code uint64
								var msg string
								for _, s := range sf {
									if s.Num == 1 && s.WireType == WireVarint {
										code = s.Varint
									} else if s.Num == 2 && s.WireType == WireBytes {
										msg = string(s.Bytes)
									}
								}
								fmt.Printf("[punctual] 스트림 상태 응답 (stream=%d, field=%d, code=%d, msg=%s)\n", streamID, w.Num, code, msg)
								if streamID == 1 || streamID == 2 || streamID == 5 || streamID == 7 {
									needsReconnect = true
								}
							}
						}
					}
				}
			}
		}
	}
	return out, needsReconnect, nil
}
