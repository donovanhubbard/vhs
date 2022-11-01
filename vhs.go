package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
)

// VHS is the object that controls the setup.
type VHS struct {
	Options      *Options
	Errors       []error
	Page         *rod.Page
	browser      *rod.Browser
	TextCanvas   *rod.Element
	CursorCanvas *rod.Element
	mutex        *sync.Mutex
	recording    bool
	tty          *exec.Cmd
	totalFrames  int
	close        func() error
}

// Options is the set of options for the setup.
type Options struct {
	FontFamily    string
	FontSize      int
	LetterSpacing float64
	LineHeight    float64
	Prompt        string
	TypingSpeed   time.Duration
	Theme         Theme
	Test          TestOptions
	Video         VideoOptions
	LoopOffset    float64
}

const (
	defaultFontSize = 22
	typingSpeed     = 50 * time.Millisecond
)

// DefaultVHSOptions returns the default set of options to use for the setup function.
func DefaultVHSOptions() Options {
	return Options{
		Prompt:        "\\[\\e[38;2;90;86;224m\\]> \\[\\e[0m\\]",
		FontFamily:    "JetBrains Mono,DejaVu Sans Mono,Menlo,Bitstream Vera Sans Mono,Inconsolata,Roboto Mono,Hack,Consolas,ui-monospace,monospace",
		FontSize:      defaultFontSize,
		LetterSpacing: 0,
		LineHeight:    1.0,
		TypingSpeed:   typingSpeed,
		Theme:         DefaultTheme,
		Video:         DefaultVideoOptions(),
	}
}

// New sets up ttyd and go-rod for recording frames.
func New() VHS {
	port := randomPort()
	tty := StartTTY(port)
	go tty.Run() //nolint:errcheck

	opts := DefaultVHSOptions()
	path, _ := launcher.LookPath()
	u := launcher.New().Leakless(false).Bin(path).MustLaunch()
	browser := rod.New().ControlURL(u).MustConnect()
	page := browser.MustPage(fmt.Sprintf("http://localhost:%d", port))

	mu := &sync.Mutex{}

	return VHS{
		Options:   &opts,
		Page:      page,
		browser:   browser,
		tty:       tty,
		recording: true,
		mutex:     mu,
		close:     browser.Close,
	}
}

// Setup sets up the VHS instance and performs the necessary actions to reflect
// the options that are default and set by the user.
func (vhs *VHS) Setup() {
	// Set Viewport to the correct size, accounting for the padding that will be
	// added during the render.
	padding := vhs.Options.Video.Padding
	width := vhs.Options.Video.Width - padding - padding
	height := vhs.Options.Video.Height - padding - padding
	vhs.Page = vhs.Page.MustSetViewport(width, height, 0, false)

	// Let's wait until we can access the window.term variable.
	vhs.Page = vhs.Page.MustWait("() => window.term != undefined")

	// Find xterm.js canvases for the text and cursor layer for recording.
	vhs.TextCanvas, _ = vhs.Page.Element("canvas.xterm-text-layer")
	vhs.CursorCanvas, _ = vhs.Page.Element("canvas.xterm-cursor-layer")

	// Set Prompt
	vhs.Page.MustElement("textarea").
		MustInput(fmt.Sprintf(` set +o history; unset PROMPT_COMMAND; export PS1="%s"; clear;`, vhs.Options.Prompt)).
		MustType(input.Enter)

	// Apply options to the terminal
	// By this point the setting commands have been executed, so the `opts` struct is up to date.
	vhs.Page.MustEval(fmt.Sprintf("() => { term.options = { fontSize: %d, fontFamily: '%s', letterSpacing: %f, lineHeight: %f, theme: %s } }",
		vhs.Options.FontSize, vhs.Options.FontFamily, vhs.Options.LetterSpacing,
		vhs.Options.LineHeight, vhs.Options.Theme.String()))

	// Fit the terminal into the window
	vhs.Page.MustEval("term.fit")

	_ = os.RemoveAll(vhs.Options.Video.Input)
	_ = os.MkdirAll(vhs.Options.Video.Input, os.ModePerm)
}

const cleanupWaitTime = 100 * time.Millisecond

// Terminate cleans up a VHS instance and terminates the go-rod browser and ttyd
// processes.
func (vhs *VHS) terminate() error {
	// Give some time for any commands executed (such as `rm`) to finish.
	//
	// If a user runs a long running command, they must sleep for the required time
	// to finish.
	time.Sleep(cleanupWaitTime)

	// Tear down the processes we started.
	vhs.browser.MustClose()
	return vhs.tty.Process.Kill()
}

// Cleanup individual frames.
func (vhs *VHS) Cleanup() error {
	if !vhs.Options.Video.CleanupFrames {
		return nil
	}

	return os.RemoveAll(vhs.Options.Video.Input)
}

// Render starts rendering the individual frames into a video.
func (vhs *VHS) Render() error {
	// Apply Loop Offset by modifying frame sequence
	vhs.ApplyLoopOffset()

	// Generate the video(s) with the frames.
	var cmds []*exec.Cmd
	cmds = append(cmds, MakeGIF(vhs.Options.Video))
	cmds = append(cmds, MakeMP4(vhs.Options.Video))
	cmds = append(cmds, MakeWebM(vhs.Options.Video))

	for _, cmd := range cmds {
		if cmd == nil {
			continue
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Println(string(out))
		}
	}

	return nil
}

// Apply Loop Offset by modifying frame sequence
func (vhs *VHS) ApplyLoopOffset() {
	loopOffsetPercentage := vhs.Options.LoopOffset

	// Calculate # of frames to offset from LoopOffset percentage
	loopOffsetFrames := int(math.Ceil(loopOffsetPercentage / 100.0 * float64(vhs.totalFrames)))

	// Take care of overflow and keep track of exact offsetPercentage
	loopOffsetFrames = loopOffsetFrames % vhs.totalFrames
	loopOffsetPercentage = float64(loopOffsetFrames) / float64(vhs.totalFrames) * 100

	// No operation if nothing to offset
	if loopOffsetFrames <= 0 {
		return
	}

	// Move all frames in [offsetStart, offsetEnd] to end of frame sequence
	offsetStart := vhs.Options.Video.StartingFrame
	offsetEnd := loopOffsetFrames

	// New starting frame will be the next frame after offsetEnd
	vhs.Options.Video.StartingFrame = offsetEnd + 1

	// Rename all text and cursor frame files in the range concurrently
	errCh := make(chan error)
	doneCh := make(chan bool)
	var wg sync.WaitGroup

	for counter := offsetStart; counter <= offsetEnd; counter++ {
		wg.Add(1)
		go func(frameNum int) {
			defer wg.Done()
			offsetFrameNum := frameNum + vhs.totalFrames
			if err := os.Rename(
				filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(cursorFrameFormat, frameNum)),
				filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(cursorFrameFormat, offsetFrameNum)),
			); err != nil {
				errCh <- fmt.Errorf("error applying offset to cursor frame: %w", err)
			}
		}(counter)

		wg.Add(1)
		go func(frameNum int) {
			defer wg.Done()
			offsetFrameNum := frameNum + vhs.totalFrames
			if err := os.Rename(
				filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(textFrameFormat, frameNum)),
				filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(textFrameFormat, offsetFrameNum)),
			); err != nil {
				errCh <- fmt.Errorf("error applying offset to text frame: %w", err)
			}
		}(counter)
	}

	go func() {
		wg.Wait()
		close(doneCh)
	}()

	select {
	case <-doneCh:
		break
	case err := <-errCh:
		// Bail out in case of an error while renaming
		fmt.Println(err)
		os.Exit(1)
	}
}

const quality = 0.92

// Record begins the goroutine which captures images from the xterm.js canvases.
func (vhs *VHS) Record(ctx context.Context) <-chan error {
	ch := make(chan error)
	interval := time.Second / time.Duration(vhs.Options.Video.Framerate)
	time.Sleep(interval)

	go func() {
		counter := 0
		for {
			select {
			case <-ctx.Done():
				_ = vhs.terminate()

				close(ch)
				// Save total # of frames for offset calculation
				vhs.totalFrames = counter
				return

			default:
				if !vhs.recording {
					time.Sleep(interval + interval)
					continue
				}

				if vhs.Page != nil {
					counter++
					start := time.Now()
					cursor, cursorErr := vhs.CursorCanvas.CanvasToImage("image/png", quality)
					text, textErr := vhs.TextCanvas.CanvasToImage("image/png", quality)
					if textErr == nil && cursorErr == nil {
						if err := os.WriteFile(
							filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(cursorFrameFormat, counter)),
							cursor,
							os.ModePerm,
						); err != nil {
							ch <- fmt.Errorf("error writing cursor frame: %w", err)
						}
						if err := os.WriteFile(
							filepath.Join(vhs.Options.Video.Input, fmt.Sprintf(textFrameFormat, counter)),
							text,
							os.ModePerm,
						); err != nil {
							ch <- fmt.Errorf("error writing text frame: %w", err)
						}
					} else {
						ch <- fmt.Errorf("error: %v, %v", textErr, cursorErr)
					}

					elapsed := time.Since(start)
					if elapsed >= interval {
						continue
					} else {
						time.Sleep(interval - elapsed)
					}
				}
			}
		}
	}()

	return ch
}

// ResumeRecording indicates to VHS that the recording should be resumed.
func (vhs *VHS) ResumeRecording() {
	vhs.mutex.Lock()
	defer vhs.mutex.Unlock()

	vhs.recording = true
}

// PauseRecording indicates to VHS that the recording should be paused.
func (vhs *VHS) PauseRecording() {
	vhs.mutex.Lock()
	defer vhs.mutex.Unlock()

	vhs.recording = false
}
