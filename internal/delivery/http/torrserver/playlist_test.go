package torrserver

import (
	"io"
	"net/url"
	"strings"
	"testing"
)

func TestM3USpecialNamesRoundTripThroughStreamHandler(t *testing.T) {
	data := []byte("verified movie")
	server, hash, _, cleanup := setupHTTPStreamServer(t, 1024, "film #?% кино", data)
	defer cleanup()
	resp, err := server.Client().Get(server.URL + "/stream?link=" + hash + "&index=1&m3u")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(resp.Status, err, string(body))
	}
	var link string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "http") {
			link = line
		}
	}
	u, err := url.Parse(link)
	if err != nil || u.Fragment != "" || u.Query().Get("link") != hash || u.Query().Get("index") != "1" {
		t.Fatal(link, err)
	}
	if !strings.Contains(u.Path, "film #?% кино.mp4") {
		t.Fatal("filename changed", u.Path)
	}
	play, err := server.Client().Get(link)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(play.Body)
	play.Body.Close()
	if err != nil || play.StatusCode != 200 || string(got) != string(data) {
		t.Fatal(play.Status, string(got), err)
	}
	if strings.ContainsAny(playlistLabel("file\r\n#EXTINF:injection"), "\r\n") {
		t.Fatal("playlist label injection")
	}
}
