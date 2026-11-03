package soundcloudx

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int64
		err  bool
	}{
		{in: "20GB", want: 20 << 30},
		{in: "512 MB", want: 512 << 20},
		{in: "1.5g", want: 3 << 29},
		{in: "10GiB", want: 10 << 30},
		{in: "1024", want: 1024},
		{in: "", err: true},
		{in: "12XB", err: true},
		{in: "-1G", err: true},
	} {
		got, err := ParseSize(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d, err %v", tt.in, got, err, tt.want, tt.err)
		}
	}
}

func TestURIRoundTrip(t *testing.T) {
	uri := buildURI("49437906", "forss/veniam")
	key, perm, ok := parseURI(uri)
	if !ok || key != "49437906" || perm != "forss/veniam" {
		t.Fatalf("parseURI(%q) = %q, %q, %v", uri, key, perm, ok)
	}
	if _, _, ok := parseURI("spotify:track:1"); ok {
		t.Fatal("foreign URI accepted")
	}
	if got := pageURL(perm); got != "https://soundcloud.com/forss/veniam" {
		t.Fatalf("pageURL = %q", got)
	}
	if got := trackKey("", "a/b c"); got != "p-a-b-c" {
		t.Fatalf("trackKey fallback = %q", got)
	}
	if got := trackKey("../x", "a/b"); got != "p-a-b" {
		t.Fatalf("unsafe id must not be a key: %q", got)
	}
}

func TestParseFlat(t *testing.T) {
	in := strings.Join([]string{
		`{"id":"1","url":"https://soundcloud.com/forss/veniam","title":"Veniam"}`,
		`{"id":"2","webpage_url":"https://soundcloud.com/other/song","title":"Song","uploader":"Other Name","duration":61.5}`,
		`{"id":"3","url":"https://soundcloud.com/forss/sets/album"}`,
		`{"id":"4","url":"https://example.com/x"}`,
		`not json`,
	}, "\n")
	got, err := parseFlat(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Meta{
		{Key: "1", Permalink: "forss/veniam", Title: "Veniam", Artist: "forss"},
		{Key: "2", Permalink: "other/song", Title: "Song", Artist: "Other Name", Duration: 61},
	}
	if len(got) != len(want) {
		t.Fatalf("parseFlat = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestTailReaderFollowsGrowingFile(t *testing.T) {
	old := tailPoll
	tailPoll = 5 * time.Millisecond
	t.Cleanup(func() { tailPoll = old })

	path := filepath.Join(t.TempDir(), "x.part")
	done := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond) // the reader waits for the file to appear
		f, _ := os.Create(path)
		for _, chunk := range []string{"abc", "def", "ghi"} {
			_, _ = f.WriteString(chunk)
			time.Sleep(15 * time.Millisecond)
		}
		f.Close()
		// The writer renames its file when it finishes; the reader keeps going.
		_ = os.Rename(path, path+".done")
		close(done)
	}()
	tr, err := openTail(path, done, make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	got, err := io.ReadAll(tr)
	if err != nil || string(got) != "abcdefghi" {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
}

func TestTailReaderCloseUnblocksRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.part")
	_ = os.WriteFile(path, nil, 0o644)
	tr, err := openTail(path, make(chan struct{}), make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := tr.Read(make([]byte, 4))
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	tr.Close()
	select {
	case err := <-errc:
		if err != errTailClosed {
			t.Fatalf("Read after Close = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return after Close")
	}
}
