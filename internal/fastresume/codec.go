package fastresume

import (
	"encoding/binary"
	"io"
)

// The binary envelope bounds every allocation before decoding. No JSON arrays,
// base64 expansion, native structs, or architecture-sized integers are persisted.
const envelopeMagic = "TSHRSM01"

type encoder struct{ data []byte }

func (e *encoder) integer(n int64) { e.data = binary.LittleEndian.AppendUint64(e.data, uint64(n)) }
func (e *encoder) text(s string) {
	e.data = binary.LittleEndian.AppendUint32(e.data, uint32(len(s)))
	e.data = append(e.data, s...)
}

func Encode(c *Checkpoint) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	e := encoder{data: []byte(envelopeMagic)}
	e.integer(int64(c.Schema))
	e.integer(c.Generation)
	e.integer(c.CreatedAtMS)
	i := c.Identity
	for _, s := range []string{i.RecordID, i.StorageID, i.Root, i.Prefix, i.MetainfoDigest, i.CatalogDigest, i.VolumeID} {
		e.text(s)
	}
	for _, n := range []int64{i.MappingRevision, int64(i.LayoutVersion), i.PieceLength, int64(i.PieceCount), i.ExtentLength, int64(len(c.Files))} {
		e.integer(n)
	}
	for _, f := range c.Files {
		e.text(f.Path)
		e.integer(f.Offset)
		e.integer(f.Length)
		present := int64(0)
		if f.Evidence.Present {
			present = 1
		}
		e.integer(present)
		e.text(f.Evidence.Identity)
		e.integer(f.Evidence.Size)
		e.integer(f.Evidence.ModifiedNS)
	}
	e.data = append(e.data, c.Pieces...)
	if len(e.data) > MaxCheckpointBytes {
		return nil, ErrBudget
	}
	return e.data, nil
}

type decoder struct {
	data []byte
	err  error
}

func (d *decoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || n > len(d.data) {
		d.err = io.ErrUnexpectedEOF
		return nil
	}
	p := d.data[:n]
	d.data = d.data[n:]
	return p
}
func (d *decoder) integer() int64 {
	p := d.take(8)
	if p == nil {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(p))
}
func (d *decoder) text(limit int) string {
	p := d.take(4)
	if p == nil {
		return ""
	}
	n := binary.LittleEndian.Uint32(p)
	if uint64(n) > uint64(limit) {
		d.err = ErrBudget
		return ""
	}
	return string(d.take(int(n)))
}

func Decode(data []byte) (*Checkpoint, error) {
	if len(data) > MaxCheckpointBytes {
		return nil, ErrBudget
	}
	d := decoder{data: data}
	if string(d.take(len(envelopeMagic))) != envelopeMagic || d.integer() != SchemaVersion {
		return nil, ErrInvalid
	}
	c := &Checkpoint{Schema: SchemaVersion, Generation: d.integer(), CreatedAtMS: d.integer()}
	i := &c.Identity
	i.RecordID, i.StorageID = d.text(MaxIdentityLength), d.text(MaxIdentityLength)
	i.Root, i.Prefix = d.text(MaxPathLength), d.text(MaxPathLength)
	i.MetainfoDigest, i.CatalogDigest, i.VolumeID = d.text(64), d.text(64), d.text(MaxIdentityLength)
	i.MappingRevision = d.integer()
	layout := d.integer()
	i.PieceLength = d.integer()
	pieces := d.integer()
	i.ExtentLength = d.integer()
	files := d.integer()
	if d.err != nil {
		return nil, d.err
	}
	if pieces < 0 || layout < 0 || layout > 2 || files < 1 {
		return nil, ErrInvalid
	}
	if pieces > MaxPieces || files > MaxFiles {
		return nil, ErrBudget
	}
	i.LayoutVersion, i.PieceCount = int(layout), int(pieces)
	if err := i.validate(); err != nil {
		return nil, err
	}
	c.Files = make([]File, int(files))
	pathBytes := 0
	for n := range c.Files {
		f := &c.Files[n]
		f.Path = d.text(min(MaxPathLength, MaxPathBytes-pathBytes))
		pathBytes += len(f.Path)
		f.Offset, f.Length = d.integer(), d.integer()
		present := d.integer()
		f.Evidence.Identity = d.text(MaxIdentityLength)
		f.Evidence.Size, f.Evidence.ModifiedNS = d.integer(), d.integer()
		if d.err != nil {
			return nil, d.err
		}
		if present != 0 && present != 1 {
			return nil, ErrInvalid
		}
		f.Evidence.Present = present == 1
	}
	bits := d.take(bitmapBytes(i.PieceCount))
	if d.err != nil {
		return nil, d.err
	}
	if len(d.data) != 0 {
		return nil, ErrInvalid
	}
	c.Pieces = append([]byte(nil), bits...)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func BitmapBytes(pieces int) (int, error) {
	if pieces < 0 || pieces > MaxPieces {
		return 0, ErrBudget
	}
	return bitmapBytes(pieces), nil
}

func ValidateBitmap(bits []byte, pieces int) error {
	if !validBitmap(bits, pieces) {
		return ErrInvalid
	}
	return nil
}
