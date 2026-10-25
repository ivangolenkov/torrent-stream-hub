package storage

import (
	"golang.org/x/sys/windows"
	"os"
	"unsafe"
)

// Native handle-relative rename, ReplaceIfExists=false. The fixed buffer is
// bounded by a single Windows filename, not an untrusted absolute path.
func renameNoReplace(dir *os.File, from, to string) error {
	return renameBetweenNoReplace(dir, dir, from, to)
}
func renameBetweenNoReplace(dir, toDir *os.File, from, to string) error {
	name, err := windows.NewNTUnicodeString(from)
	if err != nil {
		return err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(dir.Fd()), ObjectName: name, Attributes: windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, windows.DELETE|windows.SYNCHRONIZE, &oa, &status, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var info struct {
		Replace bool
		Root    windows.Handle
		Length  uint32
		Name    [windows.MAX_PATH]uint16
	}
	encoded, err := windows.UTF16FromString(to)
	if err != nil {
		return err
	}
	if len(encoded) > len(info.Name) {
		return windows.ERROR_FILENAME_EXCED_RANGE
	}
	info.Root = windows.Handle(toDir.Fd())
	info.Length = uint32((len(encoded) - 1) * 2)
	copy(info.Name[:], encoded)
	err = windows.NtSetInformationFile(h, &status, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), 10)
	if st, ok := err.(windows.NTStatus); ok {
		return st.Errno()
	}
	return err
}

// Windows does not expose a portable directory fsync. Payload is flushed before
// publication; atomic namespace replacement is provided by the filesystem.
func syncPublicationDirectory(*os.File) error { return nil }
