# gemini-remote-linux

[English](./README.md)

**리눅스 PC 및 워크스테이션을 Gemini (모바일, 웹 앱, 그리고 리눅스용 비공식 Gemini 데스크톱 앱)의 원격 실행 환경으로 연결**해 주는 비공식 Linux 데스크톱 데몬 및 CLI입니다.

단일 Go 바이너리로 로컬 폴더 연결, 터미널 명령 실행, 파일 편집, MCP 서버 연동, GUI 화면 제어(Computer Use) 기능을 Linux(Ubuntu, Arch, Fedora, Wayland/Hyprland, X11)에서 사용할 수 있으며, 스마트폰·웹 브라우저뿐만 아니라 **리눅스용 비공식 Gemini 데스크톱 앱(클라이언트/래퍼)에서도 동일하게 연동**하여 사용할 수 있습니다.

## 주요 기능

- **원격 데스크톱 연결 및 실시간 동기화**
  - 기기 등록, 온라인 상태 보고, 양방향 스트리밍 기반 원격 도구 실행 및 멀티턴 세션 자동 갱신 지원
  - 모바일(Android/iOS)·웹 Gemini 앱 및 리눅스용 비공식 데스크톱 클라이언트의 원격 호출 신호 실시간 수신 및 동기화
- **로컬 도구 전체 구현**
  - **셸 / 프로세스 / 코드**: `run_command`, `command_status`, `send_command_input`, `run_python`
  - **파일 시스템**: `read_file`, `view_file` (이미지·파일 첨부 업로드 지원), `write_file`, `replace_file_content`, `multi_replace_file_content`
  - **디렉토리 / 탐색**: `list_directory`, `find_by_name` (서브스트링 및 글로브 패턴 지원), `grep_search`, `find_folders`, `connect_folders`
  - **스킬 / MCP**: `list_skills`, `ReadSkill`, `list_mcp`, `execute_mcp` (내장 `git`, `obsidian` 및 외부 stdio MCP 서버 브리지)
  - **GUI / Computer Use**: `ui_control` (**Wayland / Hyprland** `hyprctl`, `grim`, `wtype`, `ydotool` 및 **X11** `xdotool`, `import` 지원 + 스크린샷 캡처 및 윈도우 트리 상태 보고)

## 빌드 및 실행

```bash
go build -o gemini-remote ./cmd/gemini-remote

# 0. OAuth 클라이언트 자격증명 설정 (기존 Gemini 데스크톱 앱 경로 지정 또는 ~/.config/gemini-remote/config.json 설정)
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
