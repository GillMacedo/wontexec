// Command hello is a small made-up service for the wontexec demos. It calls
// an HTTPS URL and prints the time in a named zone, so it needs CA
// certificates and time zone data at run time.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	zone := os.Getenv("REPORT_TZ")
	if zone == "" {
		zone = "Europe/Lisbon"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hello:", err)
		os.Exit(1)
	}
	fmt.Println("local time:", time.Now().In(loc).Format(time.RFC3339))
	if len(os.Args) > 1 {
		resp, err := http.Get(os.Args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "hello:", err)
			os.Exit(1)
		}
		resp.Body.Close()
		fmt.Println("status:", resp.Status)
	}
}
