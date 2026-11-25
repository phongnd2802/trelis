package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/auth"
)

// adminUsage is printed for `server admin` with bad arguments.
const adminUsage = `usage: server [-config file] admin <command>

  principal create <name>            create a Principal, print its ID
  group create <name>                create a Group, print its ID
  group add-member <group> <principal>
                                     add a Principal to a Group
  key create <principal>             issue an API key, print "<id>\t<key>"; the
                                     key is stored hashed and cannot be shown again
  key list <principal>               print "<id>\t<created>\t<active|revoked <time>>"
                                     for each key, oldest first; never the key
  key revoke <key-id>                stop that key from authenticating`

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
		issued, key, err := s.CreateKey(ctx, rest[0])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s\t%s\n", issued.ID, key)
		return err
	case "key list":
		if err := need(1); err != nil {
			return err
		}
		keys, err := s.ListKeys(ctx, rest[0])
		if err != nil {
			return err
		}
		for _, k := range keys {
			status := "active"
			if k.RevokedAt != nil {
				status = "revoked " + k.RevokedAt.UTC().Format(time.RFC3339)
			}
			if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", k.ID, k.CreatedAt.UTC().Format(time.RFC3339), status); err != nil {
				return err
			}
		}
		return nil
	case "key revoke":
		if err := need(1); err != nil {
			return err
		}
		id, err := uuid.Parse(rest[0])
		if err != nil {
			return fmt.Errorf("key ID %q: %w", rest[0], err)
		}
		return s.RevokeKey(ctx, id)
	default:
		return errUsage
	}
}
