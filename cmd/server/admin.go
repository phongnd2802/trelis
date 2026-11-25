package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/phongnd2802/trelis/internal/auth"
)

// adminUsage is printed for `server admin` with bad arguments.
const adminUsage = `usage: server [-config file] admin <command>

  principal create <name>            create a Principal, print its ID
  group create <name>                create a Group, print its ID
  group add-member <group> <principal>
                                     add a Principal to a Group
  key create <principal>             issue an API key and print it; it is
                                     stored hashed and cannot be shown again`

// errUsage marks a bad admin command line.
var errUsage = errors.New(adminUsage)

// admin runs one operator command against s and writes its result to out.
func admin(ctx context.Context, s *auth.Store, args []string, out io.Writer) error {
	cmd, rest := "", args
	if len(args) >= 2 {
		cmd, rest = args[0]+" "+args[1], args[2:]
	}
	need := func(n int) error {
		if len(rest) != n {
			return errUsage
		}
		return nil
	}

	switch cmd {
	case "principal create":
		if err := need(1); err != nil {
			return err
		}
		p, err := s.CreatePrincipal(ctx, rest[0])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, p.ID)
		return err
	case "group create":
		if err := need(1); err != nil {
			return err
		}
		g, err := s.CreateGroup(ctx, rest[0])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, g.ID)
		return err
	case "group add-member":
		if err := need(2); err != nil {
			return err
		}
		return s.AddMember(ctx, rest[0], rest[1])
	case "key create":
		if err := need(1); err != nil {
			return err
		}
		key, err := s.CreateKey(ctx, rest[0])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, key)
		return err
	default:
		return errUsage
	}
}
