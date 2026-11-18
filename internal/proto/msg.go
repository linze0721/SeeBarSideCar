// Package proto defines the peer-to-peer wire messages. Transport (framing,
// encryption, handshake) lives in conn.go.
package proto

import "magic/internal/plat"

type Type uint8

const (
	THello Type = iota + 1
	TEnter      // sender hands the pointer to the receiver at X,Y
	TLeave      // receiver hands the pointer back at X,Y
	TInput      // forwarded input while the sender controls the receiver
	TClipboard
	TLayout   // owner's windows currently spanning into its virtual display
	TWinClose // viewer: user closed the overlay of WinID; owner mutes it
	TFrame
	TFrameAck
	TWinInput  // viewer side input into the owner's window WinID
	TPing      // Seq: send time (UnixNano) the receiver echoes in TPong
	TFileOffer // sender's clipboard now holds files; manifest follows
	TFileGet   // receiver requests a byte range of one offered file
	TFileData  // sender's reply chunk
	TBye       // sender is closing the link (KCP has no FIN)
	TPong      // reply to TPing, Seq copied
	TCursor    // owner's cursor over its spanning windows, shown at X,Y; Seq 1 hides
	TClipImage // sender's clipboard now holds Image (PNG) plus Text if non-empty
	TReady     // listener adopted the link and paired the bulk session
)

type Edge uint8

const (
	EdgeLeft Edge = iota + 1
	EdgeRight
	EdgeTop
	EdgeBottom
)

// Msg is a tagged union; only the fields named for Type are set.
type Msg struct {
	Type Type

	Hello  *Hello       // THello
	X, Y   int32        // TEnter/TLeave/TInput/TCursor: cursor position in receiver coords
	Input  *plat.Event  // TInput, TWinInput (TWinInput coordinates are in sender coords)
	Text   string       // TClipboard, TClipImage
	Image  []byte       // TClipImage: PNG
	Seq    uint64       // TInput: motion fence; also TFrameAck, TPing/TPong, TCursor
	Epoch  uint32       // TEnter: sender's control epoch
	Layout []SpanWindow // TLayout
	WinID  uint64       // TWinClose/TFrame/TFrameAck/TWinInput
	Frame  *Frame       // TFrame
	Files  *FileOffer   // TFileOffer
	Chunk  *FileChunk   // TFileGet (Data empty) / TFileData
}

type Hello struct {
	Name     string
	OS       string
	Bounds   plat.Rect      // union of real monitors
	Monitors []plat.Monitor // real monitors only
	Virtual  Virtual        // sender's virtual display standing for the receiver
	// Token pairs a link's two sessions: the dialer's Hello on the control
	// session carries a fresh one, and the first message on its bulk
	// session is a Hello with only that Token (no Name).
	Token []byte
	// Version is the wire protocol version; peers of different versions
	// refuse to link. Builds before it was introduced send 0.
	Version int
}

// Version 4 adds authenticated out-of-band motion and input fences. The
// listener confirms session pairing with TReady; layouts share the frame stream.
const Version = 4

// Virtual describes the sender's virtual display. Rect is in the sender's
// coordinates; Of is the receiver monitor it mirrors, in the receiver's
// coordinates. Real is false for a phantom (display creation failed or not
// yet applied): input mapping still works, window spanning does not.
type Virtual struct {
	Rect plat.Rect
	Of   plat.Rect
	Real bool
}

// SpanWindow is an owner window intersecting its virtual display. Rect is
// the window's bounds mapped into the receiver's coordinates (it may extend
// beyond the receiver's monitor; the overlay is clipped by the OS).
type SpanWindow struct {
	ID    uint64
	App   string
	Title string
	Rect  plat.Rect
}

// Frame carries changed tiles of a window image. A Full frame resets the
// receiver's buffer to W x H before applying tiles.
type Frame struct {
	N     uint64 // monotonically increasing per window; acked via TFrameAck/Seq
	W, H  int32
	Full  bool
	Tiles []Tile
}

type Tile struct {
	X, Y int32  // top-left in frame pixels
	JPEG []byte // encoded tile; its own bounds give width/height
}

// FileOffer announces the sender's clipboard files. Entries are in transfer
// order; Path is slash-separated and relative to the transfer root, its
// first element is one of the copied top-level items.
type FileOffer struct {
	ID      uint64
	Entries []FileEntry
}

type FileEntry struct {
	Path string
	Dir  bool
	Size int64
	Mode uint32 // permission bits (0o777 mask)
}

// FileChunk is a TFileGet request (Data nil) or a TFileData reply for
// entry Index of offer ID starting at Offset. EOF marks the entry's last
// chunk.
type FileChunk struct {
	ID     uint64
	Index  int
	Offset int64
	Data   []byte
	EOF    bool
}
