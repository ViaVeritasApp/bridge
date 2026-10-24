// Command sign makes and checks the signatures bridges take a release from
// (internal/selfupdate): ed25519 over the release's SHA256SUMS.txt.
//
//	go run ./tools/sign keygen                       a new key pair
//	go run ./tools/sign sign SHA256SUMS.txt          its signature, with the private key in $BRIDGE_UPDATE_SIGNING_KEY
//	go run ./tools/sign verify SHA256SUMS.txt SHA256SUMS.txt.sig <public key>
//
// The private key goes into the release workflow's BRIDGE_UPDATE_SIGNING_KEY
// secret and nowhere else; the public one into internal/selfupdate/keys.go.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"bridge/internal/selfupdate"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("keygen, sign <file> or verify <file> <signature file> <public key>")
	}
	switch args[0] {
	case "keygen":
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		fmt.Println("private (the BRIDGE_UPDATE_SIGNING_KEY secret):", base64.StdEncoding.EncodeToString(private))
		fmt.Println("public (internal/selfupdate/keys.go):          ", base64.StdEncoding.EncodeToString(public))
		return nil
	case "sign":
		if len(args) != 2 {
			return errors.New("sign <file>")
		}
		key, err := privateKey(os.Getenv("BRIDGE_UPDATE_SIGNING_KEY"))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		fmt.Println(base64.StdEncoding.EncodeToString(ed25519.Sign(key, data)))
		return nil
	case "verify":
		if len(args) != 4 {
			return errors.New("verify <file> <signature file> <public key>")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		signature, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(args[3]))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return errors.New("the public key isn't an ed25519 key in base64")
		}
		if err := selfupdate.VerifySums(data, signature, []ed25519.PublicKey{raw}); err != nil {
			return err
		}
		fmt.Println("signed with that key")
		return nil
	}
	return fmt.Errorf("no command %q", args[0])
}

// privateKey reads a key as keygen prints it: the 64-byte key, or its 32-byte seed.
func privateKey(value string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	switch {
	case value == "":
		return nil, errors.New("BRIDGE_UPDATE_SIGNING_KEY is empty")
	case err != nil:
		return nil, errors.New("BRIDGE_UPDATE_SIGNING_KEY isn't base64")
	case len(raw) == ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	case len(raw) == ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	}
	return nil, errors.New("BRIDGE_UPDATE_SIGNING_KEY isn't an ed25519 private key")
}
