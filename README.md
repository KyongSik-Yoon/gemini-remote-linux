# gemini-remote-linux

macOS용 `Gemini.app` (`com.google.GeminiMacOS`) 프로토콜과 호환되어, **리눅스 머신을 Gemini Android / 웹 앱의 원격 데스크톱 실행 타겟으로 연결**하는 단일 바이너리 데몬 및 CLI입니다.

## 주요 기능

- **macOS `Gemini.app` 호환 기기 등록 및 실시간 시그널링**
  - `robinfrontend-pa.googleapis.com` gRPC/HTTP2 `RegisterDevice`, `UnregisterDevice`, `ListDevices`, `ReportClientPresence`, 양방향 스트리밍 `ExecuteRemoteTools` 구현 (요청 메타데이터, gzip 압축 gRPC 프레임 해제, 멀티턴 세션 스트림 자동 갱신 지원)
  - `signaler-pa.googleapis.com` Punctual WebChannel V8 (`/punctual/v1/chooseServer`, `/punctual/multi-watch/channel`) 기반 실시간 `remote-tool-wakeup` / `bard-client-sync` 수신
- **로컬 도구 전체 구현 (`internal/tools/executor.go`)**
  - 셸/프로세스/코드: `run_command`, `command_status`, `send_command_input`, `run_python`
  - 파일 시스템: `read_file`, `view_file` (Scotty 업로드 연동), `write_file` (`write_to_file`), `replace_file_content`, `multi_replace_file_content`
  - 디렉토리/탐색: `list_directory` (`list_dir`), `find_by_name` (서브스트링 및 글로브 패턴 지원), `grep_search`, `find_folders`, `connect_folders`
  - 스킬/MCP: `list_skills`, `ReadSkill` (`lookup_skill`), `list_mcp`, `execute_mcp` (내장 `git`, `obsidian` 및 외부 stdio MCP 브리지)
  - GUI / Computer Use: `ui_control` (Wayland/Hyprland `hyprctl`, `grim`, `wtype`, `ydotool` 및 X11 `xdotool`, `import` 지원 + Scotty 스크린샷 업로드 + `ui_state` 윈도우 트리 보고)

## 빌드 및 실행

```bash
go build -o gemini-remote ./cmd/gemini-remote

# 0. 공식 macOS Gemini.app 번들(또는 Contents/MacOS/Gemini 바이너리)에서 OAuth 자격증명 추출
#    (추출된 oauth_client_id / oauth_client_secret은 ~/.config/gemini-remote/config.json에 저장됨)
./gemini-remote extract-keys /path/to/Gemini.app

# 1. Google 계정 OAuth 2.0 로그인
./gemini-remote login

# 2. 허용할 로컬 작업 폴더 추가
./gemini-remote folders add ~/dev/git-repo
./gemini-remote folders add ~/Downloads

# 3. 기기 등록 확인 및 목록 조회
./gemini-remote register
./gemini-remote devices

# 4. 원격 제어 데몬 실행 (포그라운드)
./gemini-remote serve

# 5. systemd 사용자 서비스로 상시 실행 등록
./gemini-remote service install
```
