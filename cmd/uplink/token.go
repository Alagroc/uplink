package main

import (
	"fmt"
	"os"
)

func runToken(args []string) error {
	fs := flagSet("token", "print the operator token that crew and capcom need")
	create := fs.Bool("create", false, "generate the token file if it does not exist")
	file := fs.String("token-file", "", "read the token from this file instead of ~/.uplink/token")
	if err := fs.Parse(args); err != nil {
		return err
	}

	tok, err := loadToken(*file, *create)
	if err != nil {
		return err
	}
	// The token goes to stdout so it can be captured; the warning goes to
	// stderr so it does not end up inside `$(uplink token)`.
	fmt.Fprintln(os.Stderr, "# treat this as a password: it authorises command execution on every crew")
	fmt.Println(tok)
	return nil
}
