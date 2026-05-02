package main

import (
	"fmt"
	"github.com/anacrolix/torrent"
	"reflect"
)

func main() {
	t := reflect.TypeOf(torrent.ClientConfig{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		fmt.Printf("%s %s\n", f.Name, f.Type)
	}
}
