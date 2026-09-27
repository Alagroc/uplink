package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Alagroc/uplink/internal/ground"
)

const crewTokenUsage = `uplink crew-token — manage the credentials crew use to reach ground

Usage:
  uplink crew-token add <name> [--role <role>]...
  uplink crew-token list
  uplink crew-token revoke <name>

A crew token opens only the crew endpoints, and only for the one crew name it
was minted for. It cannot dispatch jobs, read the inbox, or stop ground — that
is what the operator token is for.

Run these on the machine that runs ground; they edit its state directly, so they
work whether or not ground is running, and take effect immediately.
`

func runCrewToken(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, crewTokenUsage)
		return fmt.Errorf("expected add, list or revoke")
	}

	switch args[0] {
	case "add":
		return crewTokenAdd(args[1:])
	case "list", "ls":
		return crewTokenList(args[1:])
	case "revoke", "rm":
		return crewTokenRevoke(args[1:])
	case "-h", "--help", "help":
		fmt.Print(crewTokenUsage)
		return nil
	}
	fmt.Fprint(os.Stderr, crewTokenUsage)
	return fmt.Errorf("unknown subcommand %q", args[0])
}

// crewTokenStore opens the store beside ground's other state.
func crewTokenStore(stateDir string) (*ground.CrewTokenStore, error) {
	dir := stateDir
	if dir == "" {
		home, err := homeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, "ground")
	}
	return ground.OpenCrewTokens(filepath.Join(dir, "crew-tokens.json"))
}

// takeName pulls a leading positional argument out before flag parsing.
//
// Go's flag package stops at the first non-flag argument, so without this
// `crew-token add devbox --role builder` would leave the flags unparsed. Both
// orderings now work, because both are things people type.
func takeName(args []string) (name string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func crewTokenAdd(args []string) error {
	name, args := takeName(args)
	fs := flagSet("crew-token add", "mint a credential for one crew")
	var roles stringList
	fs.Var(&roles, "role", "role this crew may claim; repeatable. When set, the token overrides whatever the crew declares")
	stateDir := fs.String("state", "", "ground state directory (default ~/.uplink/ground)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" {
		if fs.NArg() != 1 {
			fs.Usage()
			return fmt.Errorf("give exactly one crew name")
		}
		name = fs.Arg(0)
	}

	store, err := crewTokenStore(*stateDir)
	if err != nil {
		return err
	}
	token, err := store.Mint(name, roles)
	if err != nil {
		return err
	}

	// The secret goes to stdout so it can be captured; everything else goes to
	// stderr so `$(uplink crew-token add x)` yields just the token.
	fmt.Fprintf(os.Stderr, "Minted a crew token for %q", name)
	if len(roles) > 0 {
		fmt.Fprintf(os.Stderr, " with roles: %s", strings.Join(roles, ", "))
	}
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Println(token)
	fmt.Fprintf(os.Stderr, "\nOnly the hash is stored, so this will not be shown again.\n")
	fmt.Fprintf(os.Stderr, "On that crew host:\n")
	fmt.Fprintf(os.Stderr, "  export UPLINK_TOKEN=%s\n", token)
	fmt.Fprintf(os.Stderr, "  %s crew --name %s --workdir /path/to/project\n", selfName(), name)
	fmt.Fprintf(os.Stderr, "\nRevoke it at any time with: %s crew-token revoke %s\n", selfName(), name)
	return nil
}

func crewTokenList(args []string) error {
	fs := flagSet("crew-token list", "list crew credentials")
	stateDir := fs.String("state", "", "ground state directory (default ~/.uplink/ground)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, err := crewTokenStore(*stateDir)
	if err != nil {
		return err
	}
	tokens := store.List()
	if len(tokens) == 0 {
		fmt.Printf("No crew tokens yet. Mint one with:  %s crew-token add <name>\n", selfName())
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CREW\tROLES\tCREATED\tSTATUS")
	for _, t := range tokens {
		roles := strings.Join(t.Roles, ",")
		if roles == "" {
			roles = "-"
		}
		status := "active"
		if t.Revoked() {
			status = "revoked " + t.RevokedAt.Format("2006-01-02")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Name, roles, t.CreatedAt.Local().Format("2006-01-02 15:04"), status)
	}
	return w.Flush()
}

func crewTokenRevoke(args []string) error {
	name, args := takeName(args)
	fs := flagSet("crew-token revoke", "withdraw a crew credential")
	stateDir := fs.String("state", "", "ground state directory (default ~/.uplink/ground)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" {
		if fs.NArg() != 1 {
			fs.Usage()
			return fmt.Errorf("give exactly one crew name")
		}
		name = fs.Arg(0)
	}

	store, err := crewTokenStore(*stateDir)
	if err != nil {
		return err
	}
	if err := store.Revoke(name); err != nil {
		return err
	}
	fmt.Printf("Revoked the token for crew %q at %s.\n", name, time.Now().Format(time.RFC3339))
	fmt.Printf("It stops working on that crew's next request; no ground restart needed.\n")
	return nil
}
