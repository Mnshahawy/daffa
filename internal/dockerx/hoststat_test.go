package dockerx

import "testing"

func TestParseHostDisk(t *testing.T) {
	// A realistic `df -P -k /host`: a header row then exactly one filesystem row (the -P/path-arg
	// form never wraps or lists more). KB are scaled to bytes.
	out := "Filesystem           1024-blocks      Used Available Capacity Mounted on\n" +
		"/dev/root               30840584   5242880  25597704      17% /host\n"

	d, err := parseHostDisk(out)
	if err != nil {
		t.Fatalf("parseHostDisk: %v", err)
	}
	if d.Total != 30840584*1024 || d.Used != 5242880*1024 || d.Free != 25597704*1024 {
		t.Fatalf("got total=%d used=%d free=%d", d.Total, d.Used, d.Free)
	}
}

func TestParseHostDiskNoRow(t *testing.T) {
	// Header only (the probe ran but df printed nothing usable): an error, not a zero-valued disk
	// that would render as a full bar.
	if _, err := parseHostDisk("Filesystem 1024-blocks Used Available Capacity Mounted on\n"); err == nil {
		t.Fatal("expected an error when no filesystem row is present")
	}
}
