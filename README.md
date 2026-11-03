# gemini-remote-linux

[한국어 문서 (Korean)](./README.ko.md)

A single-binary daemon and CLI compatible with the macOS `Gemini.app` (`com.google.GeminiMacOS`) protocol, connecting a **Linux machine as a remote desktop execution target for the Gemini Android and web apps**.

## Key Features

- **macOS `Gemini.app`-compatible Device Registration & Real-time Signaling**
  - `robinfrontend-pa.googleapis.com` gRPC/HTTP2 implementation of `RegisterDevice`, `UnregisterDevice`, `ListDevices`, `ReportClientPresence`, and bidirectional streaming `ExecuteRemoteTools` (including request metadata, gzip-compressed gRPC frame decompression, and automatic multi-turn session stream renewal)
  - `signaler-pa.googleapis.com` Punctual WebChannel V8 (`/punctual/v1/chooseServer`, `/punctual/multi-watch/channel`) real-time reception of `remote-tool-wakeup` / `bard-client-sync`
- **Full Local Tool Execution (`internal/tools/executor.go`)**
  - Shell / Process / Code: `run_command`, `command_status`, `send_command_input`, `run_python`
  - File System: `read_file`, `view_file` (with Scotty upload integration), `write_file` (`write_to_file`), `replace_file_content`, `multi_replace_file_content`
  - Directory / Search: `list_directory` (`list_dir`), `find_by_name` (substring and glob pattern support), `grep_search`, `find_folders`, `connect_folders`
  - Skills / MCP: `list_skills`, `ReadSkill` (`lookup_skill`), `list_mcp`, `execute_mcp` (built-in `git`, `obsidian`, plus external stdio MCP bridge)
  - GUI / Computer Use: `ui_control` (Wayland/Hyprland via `hyprctl`, `grim`, `wtype`, `ydotool` and X11 via `xdotool`, `import` + Scotty screenshot upload + `ui_state` window tree reporting)

## Build & Usage

```bash
go build -o gemini-remote ./cmd/gemini-remote

# 0. Extract OAuth credentials from the official macOS Gemini.app bundle (or Contents/MacOS/Gemini binary)
#    (Extracted oauth_client_id / oauth_client_secret are saved to ~/.config/gemini-remote/config.json)
./gemini-remote extract-keys /path/to/Gemini.app

# 1. Sign in with Google OAuth 2.0
./gemini-remote login

# 2. Allow local workspace folders
./gemini-remote folders add ~/dev/git-repo
./gemini-remote folders add ~/Downloads

# 3. Register device and list registered devices
./gemini-remote register
./gemini-remote devices

# 4. Run remote control daemon (foreground)
./gemini-remote serve

# 5. Install and start as a systemd user service
./gemini-remote service install
```
