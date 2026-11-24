package llm

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestValidateImageData(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := ValidateImageData("image/png", b.Bytes()); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, []byte("fake png"), {0xff, 0xfe, 'P', 0}, b.Bytes()[:b.Len()/2]} {
		if err := ValidateImageData("image/png", data); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
	if err := ValidateImageData("image/jpeg", b.Bytes()); err == nil {
		t.Fatal("MIME mismatch accepted")
	}
}
