package storage

import (
	"golang.org/x/sys/windows"
	"os"
)

func FileHasMultipleLinks(f *os.File, _ os.FileInfo) bool {
	var info windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info)
	return err != nil || info.NumberOfLinks > 1
}
