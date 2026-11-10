package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strings"
)

const logoCols, logoRows = 10, 5

// kittyOK is true in kitty outside tmux, where the graphics protocol works.
func kittyOK() bool {
	if os.Getenv("TMUX") != "" {
		return false
	}
	return os.Getenv("KITTY_WINDOW_ID") != "" || os.Getenv("TERM") == "xterm-kitty"
}

// logoFrames decodes the GIF at path into full PNG frames and their delays in ms.
func logoFrames(path string) ([][]byte, []int, error) {
	if strings.HasPrefix(path, "~/") {
		h, _ := os.UserHomeDir()
		path = filepath.Join(h, path[2:])
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	g, err := gif.DecodeAll(f)
	if err != nil {
		return nil, nil, err
	}
	canvas := image.NewRGBA(image.Rect(0, 0, g.Config.Width, g.Config.Height))
	var frames [][]byte
	var delays []int
	for i, fr := range g.Image {
		prev := image.NewRGBA(canvas.Bounds())
		copy(prev.Pix, canvas.Pix)
		draw.Draw(canvas, fr.Bounds(), fr, fr.Bounds().Min, draw.Over)
		var b bytes.Buffer
		if err := png.Encode(&b, canvas); err != nil {
			return nil, nil, err
		}
		frames = append(frames, b.Bytes())
		d := 100
		if i < len(g.Delay) && g.Delay[i] > 0 {
			d = g.Delay[i] * 10
		}
		delays = append(delays, d)
		if i < len(g.Disposal) {
			switch g.Disposal[i] {
			case gif.DisposalBackground:
				draw.Draw(canvas, fr.Bounds(), image.Transparent, image.Point{}, draw.Src)
			case gif.DisposalPrevious:
				canvas = prev
			}
		}
	}
	return frames, delays, nil
}

// kittyCmd writes one graphics command with its whole payload. kitty
// 0.41 does not animate frames sent in 4096-byte chunks.
func kittyCmd(b *strings.Builder, ctrl string, data []byte) {
	fmt.Fprintf(b, "\x1b_G%s,q=2;%s\x1b\\", ctrl, base64.StdEncoding.EncodeToString(data))
}

// kittyLogo returns the escape codes that show the animated logo at the
// cursor, logoCols x logoRows cells, without moving the cursor.
func kittyLogo(frames [][]byte, delays []int) string {
	const id = 4242
	var b strings.Builder
	fmt.Fprintf(&b, "\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id)
	kittyCmd(&b, fmt.Sprintf("a=T,f=100,i=%d,c=%d,r=%d,C=1", id, logoCols, logoRows), frames[0])
	fmt.Fprintf(&b, "\x1b_Ga=a,i=%d,r=1,z=%d,q=2\x1b\\", id, delays[0])
	for i, fr := range frames[1:] {
		kittyCmd(&b, fmt.Sprintf("a=f,f=100,i=%d,z=%d", id, delays[i+1]), fr)
	}
	fmt.Fprintf(&b, "\x1b_Ga=a,i=%d,s=3,v=1,q=2\x1b\\", id)
	return b.String()
}
