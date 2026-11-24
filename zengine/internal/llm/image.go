package llm

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// ValidateImageData checks common decodable image formats before attaching them.
// Other image formats retain their existing provider-specific support.
func ValidateImageData(mime string, data []byte) error {
	if len(data) >= 2 && (bytes.Equal(data[:2], []byte{0xff, 0xfe}) || bytes.Equal(data[:2], []byte{0xfe, 0xff})) {
		return fmt.Errorf("image contains UTF-16 text, not image bytes; capture binary output without PowerShell text redirection (for adb use adb shell screencap -p /sdcard/screen.png, then adb pull /sdcard/screen.png)")
	}
	format := map[string]string{"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif"}[mime]
	if format == "" {
		return nil
	}
	cfg, actual, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid %s image: %w", mime, err)
	}
	if actual != format {
		return fmt.Errorf("image bytes are %s, but declared MIME is %s", actual, mime)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 64_000_000 {
		return fmt.Errorf("image dimensions %dx%d exceed the 64 megapixel decoding limit; use a smaller copy", cfg.Width, cfg.Height)
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("corrupt or truncated %s image: %w", mime, err)
	}
	return nil
}
