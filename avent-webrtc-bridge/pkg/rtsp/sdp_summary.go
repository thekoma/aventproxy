package rtsp

import (
	"fmt"
	"strings"
)

// summarizeSDP describes the media sections of a session description and
// nothing else: kind, mid, direction and codec names. ICE credentials, the
// DTLS fingerprint, candidates, SSRCs and the LAN path's aes-key never
// appear, so the result is safe to log. It reads line by line on purpose, so
// a quirky answer from the camera cannot defeat it.
func summarizeSDP(raw string) string {
	type section struct {
		kind, mid, dir string
		codecs         []string
	}
	var sections []*section
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "m="):
			kind := "?"
			if f := strings.Fields(line[2:]); len(f) > 0 {
				kind = f[0]
			}
			sections = append(sections, &section{kind: kind, dir: "sendrecv"}) // RFC 4566 default
		case len(sections) == 0:
			continue
		case strings.HasPrefix(line, "a=mid:"):
			sections[len(sections)-1].mid = strings.TrimPrefix(line, "a=mid:")
		case line == "a=sendrecv", line == "a=sendonly", line == "a=recvonly", line == "a=inactive":
			sections[len(sections)-1].dir = line[2:]
		case strings.HasPrefix(line, "a=rtpmap:"):
			// imm's rtpmap has a third field: "a=rtpmap:6001 AES/KCP 330".
			if f := strings.Fields(line); len(f) >= 2 {
				sections[len(sections)-1].codecs = append(sections[len(sections)-1].codecs, f[1])
			}
		}
	}
	if len(sections) == 0 {
		return fmt.Sprintf("no media sections (%d bytes)", len(raw))
	}
	parts := make([]string, 0, len(sections))
	for _, s := range sections {
		parts = append(parts, fmt.Sprintf("%s(mid=%s %s %s)", s.kind, s.mid, s.dir, strings.Join(s.codecs, ",")))
	}
	return strings.Join(parts, " ")
}
