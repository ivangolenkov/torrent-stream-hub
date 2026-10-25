package storage

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
)

func FileIdentity(f *os.File) (string, error) {
	var v windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &v); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x:%x", v.VolumeSerialNumber, v.FileIndexHigh, v.FileIndexLow), nil
}
