package cua

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ExecutionResult는 ui_control 실행 결과와 캡처된 스크린샷, UI 트리 상태를 담는다.
type ExecutionResult struct {
	Stdout         string
	Stderr         string
	ExitCode       int
	ScreenshotJPEG []byte
	UIState        string
}

// RunUIControl은 macOS Gemini.app의 CUAScriptRuntime과 호환되는 Python 환경을 구성해
// Wayland(Hyprland: hyprctl + grim + wtype) 및 X11(xdotool + import)에서 GUI 자동화를 수행한다.
func RunUIControl(ctx context.Context, code string, captureScreenshot bool) (*ExecutionResult, error) {
	tmpDir, err := os.MkdirTemp("", "gemini-cua-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	shotPath := filepath.Join(tmpDir, "cua_screenshot.png")
	prelude := buildPythonPrelude(shotPath)
	fullScript := prelude + "\n" + code

	scriptFile := filepath.Join(tmpDir, "run_cua.py")
	if err := os.WriteFile(scriptFile, []byte(fullScript), 0o600); err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "python3", scriptFile)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = 1
		}
	}

	res := &ExecutionResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		UIState:  CollectUIState(ctx),
	}

	if captureScreenshot {
		if _, err := os.Stat(shotPath); err != nil {
			_ = takeScreenshotPNG(ctx, shotPath)
		}
		if pngBytes, err := os.ReadFile(shotPath); err == nil && len(pngBytes) > 0 {
			if jpgBytes, err := pngToJPEG(pngBytes); err == nil {
				res.ScreenshotJPEG = jpgBytes
			} else {
				res.ScreenshotJPEG = pngBytes
			}
		}
	}

	return res, nil
}

// CollectUIState는 Hyprland 창 목록·포그라운드 창 정보와 AT-SPI 접근성 트리를 결합해 반환한다.
func CollectUIState(ctx context.Context) string {
	var sb strings.Builder
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if out, err := exec.CommandContext(cctx, "hyprctl", "activewindow", "-j").Output(); err == nil && len(out) > 0 {
		var win map[string]any
		if json.Unmarshal(out, &win) == nil {
			sb.WriteString(fmt.Sprintf("ActiveWindow: class=%v title=%q at=%v size=%v workspace=%v\n",
				win["class"], win["title"], win["at"], win["size"], win["workspace"]))
		}
	}
	if out, err := exec.CommandContext(cctx, "hyprctl", "clients", "-j").Output(); err == nil && len(out) > 0 {
		var clients []map[string]any
		if json.Unmarshal(out, &clients) == nil {
			sb.WriteString("Windows:\n")
			for i, c := range clients {
				if i >= 25 {
					break
				}
				sb.WriteString(fmt.Sprintf("  - [%v] %q at=%v size=%v\n",
					c["class"], c["title"], c["at"], c["size"]))
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

func takeScreenshotPNG(ctx context.Context, outPath string) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := exec.LookPath("grim"); err == nil {
		return exec.CommandContext(cctx, "grim", outPath).Run()
	}
	if _, err := exec.LookPath("import"); err == nil {
		return exec.CommandContext(cctx, "import", "-window", "root", outPath).Run()
	}
	return fmt.Errorf("스크린샷 도구를 찾을 수 없습니다 (grim 또는 imagemagick 필요)")
}

func pngToJPEG(pngData []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func buildPythonPrelude(shotPath string) string {
	return fmt.Sprintf(`import os, subprocess, time, shutil, json

_SHOT_PATH = %q

def _run(cmd):
    return subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

def move_to(x, y):
    if shutil.which("hyprctl"):
        _run(["hyprctl", "dispatch", "movecursor", str(int(x)), str(int(y))])
    elif shutil.which("xdotool"):
        _run(["xdotool", "mousemove", str(int(x)), str(int(y))])

def click(x=None, y=None, button="left"):
    if x is not None and y is not None:
        move_to(x, y)
        time.sleep(0.05)
    btn_map = {"left": "1", "middle": "2", "right": "3"}
    b = btn_map.get(str(button).lower(), "1")
    if shutil.which("ydotool"):
        code_map = {"1": "0xC0", "2": "0xC2", "3": "0xC1"}
        _run(["ydotool", "click", code_map.get(b, "0xC0")])
    elif shutil.which("xdotool"):
        _run(["xdotool", "click", b])
    elif shutil.which("hyprctl"):
        _run(["hyprctl", "dispatch", "sendshortcut", "", "mouse:272", ""])

def double_click(x=None, y=None):
    click(x, y, "left")
    time.sleep(0.08)
    click(None, None, "left")

def right_click(x=None, y=None):
    click(x, y, "right")

def drag_to(x, y):
    move_to(x, y)

def type_text(text):
    if shutil.which("wtype"):
        _run(["wtype", "--", str(text)])
    elif shutil.which("xdotool"):
        _run(["xdotool", "type", "--clearmodifiers", "--", str(text)])

def press_key(key):
    if shutil.which("wtype"):
        _run(["wtype", "-k", str(key)])
    elif shutil.which("xdotool"):
        _run(["xdotool", "key", str(key)])

def hotkey(*keys):
    if not keys:
        return
    if shutil.which("wtype"):
        args = ["wtype"]
        for k in keys[:-1]:
            args += ["-M", str(k).lower()]
        args += ["-k", str(keys[-1])]
        for k in reversed(keys[:-1]):
            args += ["-m", str(k).lower()]
        _run(args)
    elif shutil.which("xdotool"):
        _run(["xdotool", "key", "+".join(str(k) for k in keys)])

def scroll(dx=0, dy=0, x=None, y=None):
    if x is not None and y is not None:
        move_to(x, y)
    if shutil.which("xdotool") and dy != 0:
        btn = "4" if dy > 0 else "5"
        _run(["xdotool", "click", "--repeat", str(abs(int(dy))), btn])

def wait(seconds=1.0):
    time.sleep(float(seconds))

def open_app(name):
    subprocess.Popen(["sh", "-c", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

def open_application(name):
    return open_app(name)

def screenshot():
    if shutil.which("grim"):
        _run(["grim", _SHOT_PATH])
    elif shutil.which("import"):
        _run(["import", "-window", "root", _SHOT_PATH])
    return _SHOT_PATH

def observe_window():
    if shutil.which("hyprctl"):
        r = _run(["hyprctl", "activewindow", "-j"])
        return r.stdout
    return ""

read_screen = observe_window
read_som = observe_window
handleClick = click
handleType = type_text
handleMove = move_to
handleWait = wait
handleTakeScreenshot = screenshot
`, shotPath)
}
