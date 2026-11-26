// fakeserver runs the fake Amazon Photos API as a standalone process, to try
// the backup (or its Docker image) end to end without a real account.
//
//	go run ./internal/amazon/amazontest/fakeserver
//
// It prints the AMAZON_BASE_URL to use and a cookie string it accepts.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

	"amazon-photos-backup/internal/amazon/amazontest"
)

type reporter struct{}

func (reporter) Errorf(format string, args ...any) {
	log.Printf("FAKE SERVER CHECK FAILED: "+format, args...)
}
func (reporter) Cleanup(func()) {}

func main() {
	f := amazontest.New(reporter{})
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("demoNode%04d", i)
		date := fmt.Sprintf("20%02d-%02d-15T12:00:00Z", 20+i%4, 1+i%12)
		f.AddNode(id, fmt.Sprintf("IMG_%04d.jpg", i%20), date, []byte(strings.Repeat(id+"\n", 2000)))
	}
	var cookies []string
	for k, v := range f.Cookies() {
		cookies = append(cookies, k+"="+v)
	}
	fmt.Printf("AMAZON_BASE_URL=%s\n", f.URL())
	fmt.Printf("COOKIES=%s\n", strings.Join(cookies, "; "))
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
}
