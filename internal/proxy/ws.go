package proxy

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const maxWSMessage = 1 << 20 // a larger frame passes through unread

// wsFilter edits the server's side of a websocket: each whole, unmasked,
// uncompressed text frame goes through edit, which returns a new payload or
// nil to keep the frame as it is. Every other frame passes byte for byte.
type wsFilter struct {
	io.ReadWriteCloser
	br     *bufio.Reader
	edit   func([]byte) []byte
	out    []byte // bytes ready for the player
	remain int64  // payload bytes of a passed-through frame still to copy
}

func newWSFilter(conn io.ReadWriteCloser, edit func([]byte) []byte) *wsFilter {
	return &wsFilter{ReadWriteCloser: conn, br: bufio.NewReader(conn), edit: edit}
}

func (f *wsFilter) Read(p []byte) (int, error) {
	for len(f.out) == 0 {
		if f.remain > 0 {
			n, err := f.br.Read(p[:min(int64(len(p)), f.remain)])
			f.remain -= int64(n)
			return n, err
		}
		if err := f.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

// next reads one frame header, and the payload of a frame it may edit.
func (f *wsFilter) next() error {
	hdr := make([]byte, 2, 14)
	if _, err := io.ReadFull(f.br, hdr); err != nil {
		return err
	}
	size := int64(hdr[1] & 0x7f)
	if ext := map[int64]int{126: 2, 127: 8}[size]; ext > 0 {
		hdr = hdr[:2+ext]
		if _, err := io.ReadFull(f.br, hdr[2:]); err != nil {
			return err
		}
		if ext == 2 {
			size = int64(binary.BigEndian.Uint16(hdr[2:]))
		} else {
			size = int64(binary.BigEndian.Uint64(hdr[2:]))
		}
	}
	masked := hdr[1]&0x80 != 0
	if masked {
		hdr = append(hdr, 0, 0, 0, 0)
		if _, err := io.ReadFull(f.br, hdr[len(hdr)-4:]); err != nil {
			return err
		}
	}
	// FIN, no RSV bits, opcode text.
	if hdr[0] != 0x81 || masked || size > maxWSMessage {
		f.out, f.remain = hdr, size
		return nil
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(f.br, payload); err != nil {
		return err
	}
	if edited := f.edit(payload); edited != nil {
		f.out = append(wsHeader(len(edited)), edited...)
	} else {
		f.out = append(hdr, payload...)
	}
	return nil
}

// wsHeader frames a whole, unmasked text message of n bytes.
func wsHeader(n int) []byte {
	switch {
	case n < 126:
		return []byte{0x81, byte(n)}
	case n <= 0xffff:
		return binary.BigEndian.AppendUint16([]byte{0x81, 126}, uint16(n))
	default:
		return binary.BigEndian.AppendUint64([]byte{0x81, 127}, uint64(n))
	}
}

// isWebsocket reports whether r asks to upgrade to a websocket.
func isWebsocket(h http.Header) bool { return strings.EqualFold(h.Get("Upgrade"), "websocket") }

// userDataMessage rewrites the server's UserDataChanged push with the local
// user data, so a change on the shared account never reaches the player.
func (s *Server) userDataMessage(payload []byte) []byte {
	var msg struct {
		MessageType string
		Data        struct {
			UserID       string           `json:"UserId"`
			UserDataList []map[string]any `json:"UserDataList"`
		}
	}
	if json.Unmarshal(payload, &msg) != nil || msg.MessageType != "UserDataChanged" {
		return nil
	}
	for i, old := range msg.Data.UserDataList {
		if id := str(old["ItemId"]); id != "" {
			msg.Data.UserDataList[i] = userData(itemID(id), s.profile.Get(itemID(id)), old)
		}
	}
	raw, _ := json.Marshal(msg)
	return raw
}
