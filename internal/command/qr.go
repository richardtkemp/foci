package command

import (
	"fmt"
	"os"

	qrcode "github.com/skip2/go-qrcode"

	"foci/internal/tempdir"
)

// pairingQRFile renders data as a QR-code PNG in a temp file and returns its
// path. Callers hand the path back via Response.DocPath (or a wizard's
// PendingDoc); the platform layer sends and then removes it.
func pairingQRFile(data string) (string, error) {
	png, err := qrcode.Encode(data, qrcode.Medium, 512)
	if err != nil {
		return "", fmt.Errorf("qr encode: %w", err)
	}
	f, err := tempdir.Create("foci-pair-*.png")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(png); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
