# gemini-remote-linux

[한국어 문서 (Korean)](./README.ko.md)

An unofficial Linux desktop daemon and CLI that connects your **Linux PC or workstation to Gemini (Mobile, Web, and unofficial Linux Gemini desktop apps)** for remote tool execution, local workspace access, shell commands, MCP servers, and GUI Computer Use.

Bring the **Gemini Desktop** experience to Linux (Ubuntu, Arch, Fedora, Wayland/Hyprland, and X11) in a single lightweight Go binary — whether you are controlling your machine remotely from your phone, from the browser, or directly from an **unofficial Gemini desktop client on Linux**.

## Key Features

- **Remote Desktop Connection & Real-time Sync**
  - Device registration, presence reporting, and bidirectional streaming tool execution with automatic multi-turn session renewal
  - Real-time wake-up and session synchronization across Gemini mobile apps (Android/iOS), the Gemini web app, and unofficial Linux Gemini desktop clients/wrappers
- **Full Local Tool Execution**
  - **Shell / Process / Code**: `run_command`, `command_status`, `send_command_input`, `run_python`
  - **File System**: `read_file`, `view_file` (with image/file attachment upload), `write_file`, `replace_file_content`, `multi_replace_file_content`
  - **Directory / Search**: `list_directory`, `find_by_name` (substring and glob pattern support), `grep_search`, `find_folders`, `connect_folders`
  - **Skills / MCP**: `list_skills`, `ReadSkill`, `list_mcp`, `execute_mcp` (built-in `git`, `obsidian`, plus external stdio MCP server bridge)
  - **GUI / Computer Use**: `ui_control` supporting both **Wayland / Hyprland** (`hyprctl`, `grim`, `wtype`, `ydotool`) and **X11** (`xdotool`, `import`) with screenshot capture and window tree reporting

## Build & Quick Start

```bash
go build -o gemini-remote ./cmd/gemini-remote

# 0. Configure OAuth client credentials (from an existing Gemini desktop installation or ~/.config/gemini-remote/config.json)
./gemini-remote extract-keys /path/to/Gemini.app

# 1. Sign in with Google OAuth 2.0
./gemini-remote login

# 2. Allow local workspace folders
./gemini-remote folders add ~/dev/git-repo
./gemini-remote folders add ~/Downloads

# 3. Register your Linux machine and verify device list
./gemini-remote register
./gemini-remote devices

# 4. Run the daemon in the foreground
./gemini-remote serve

# 5. Or install and start as a background systemd user service
./gemini-remote service install
```
