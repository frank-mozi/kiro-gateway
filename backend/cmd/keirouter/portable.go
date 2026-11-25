package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mydisha/keirouter/backend/internal/portable"
	"github.com/mydisha/keirouter/backend/internal/version"
)

func cmdPortable(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("portable", flag.ContinueOnError)
	root := fs.String("root", filepath.Dir(exe), "portable root containing the original executable and frontend")
	port := fs.Int("port", 20180, "loopback gateway port (1024-65535)")
	noBrowser := fs.Bool("no-browser", false, "do not open the dashboard")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected portable arguments")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return portable.Run(ctx, *root, *port, version.Resolve(Version), func(url string) {
		fmt.Printf("Portable Kiro Gateway: %s\nNo sudo/systemd required. Keep this launcher running; Ctrl+C stops the gateway.\n", url)
		if !*noBrowser {
			openBrowser(url)
		}
	})
}

func cmdRelease(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("kiro-gateway release keygen --out <private-key-file>")
		fmt.Println("kiro-gateway release publish --bundle <directory> --out <feed-directory> --version X.Y.Z --os linux|windows|darwin --arch amd64|arm64 --key <private-key-file> [--notes <text>]")
		fmt.Println("Never put the private signing key in a release, source control or shared folder.")
		return nil
	}
	switch args[0] {
	case "keygen":
		fs := flag.NewFlagSet("release keygen", flag.ContinueOnError)
		out := fs.String("out", "", "private key destination (must not exist)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *out == "" || fs.NArg() != 0 {
			return errors.New("--out is required")
		}
		public, err := portable.GenerateKey(*out)
		if err != nil {
			return err
		}
		fmt.Println("Publisher public key:", public)
		return nil
	case "publish":
		fs := flag.NewFlagSet("release publish", flag.ContinueOnError)
		bundle := fs.String("bundle", "", "built executable and frontend directory")
		out := fs.String("out", "", "release feed directory")
		ver := fs.String("version", "", "stable X.Y.Z release version")
		goos := fs.String("os", "", "target OS")
		arch := fs.String("arch", "", "target architecture")
		key := fs.String("key", "", "private signing key")
		notes := fs.String("notes", "", "release notes")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *bundle == "" || *out == "" || *key == "" || fs.NArg() != 0 {
			return errors.New("--bundle, --out and --key are required")
		}
		if err := portable.Publish(*bundle, *out, *ver, *goos, *arch, *key, *notes); err != nil {
			return err
		}
		fmt.Println("Published signed release", *ver, "to", *out)
		return nil
	default:
		return errors.New("unknown release command; use release --help")
	}
}
