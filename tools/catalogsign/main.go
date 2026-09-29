// Command catalogsign generates the Barn catalog signing keypair and signs
// catalog files with it, using the same minisign implementation the CLI
// verifies with. The repository layout and rotation policy live in
// https://barn.pgsty.com/docs/reference/images/; the verifier embeds the
// active and standby PUBLIC keys in internal/image/keys.go.
//
// The private-key password is read from CATALOGSIGN_PASSWORD (empty allowed).
//
//	catalogsign generate <dir> <name>      write <name>.key/<name>.pub, print the public key
//	catalogsign sign <key> <file>...       write <file>.minisig beside each file
//	catalogsign verify <pub> <file>...     verify <file> against <file>.minisig
package main

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"aead.dev/minisign"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) < 1 {
		return fmt.Errorf("usage: catalogsign generate|sign|verify")
	}
	password := os.Getenv("CATALOGSIGN_PASSWORD")
	switch arguments[0] {
	case "generate":
		if len(arguments) != 3 {
			return fmt.Errorf("usage: catalogsign generate <dir> <name>")
		}
		return generate(arguments[1], arguments[2], password)
	case "sign":
		if len(arguments) < 3 {
			return fmt.Errorf("usage: catalogsign sign <key> <file>")
		}
		return sign(arguments[1], password, arguments[2:])
	case "verify":
		if len(arguments) < 3 {
			return fmt.Errorf("usage: catalogsign verify <pub> <file>")
		}
		return verify(arguments[1], arguments[2:])
	default:
		return fmt.Errorf("unknown subcommand %q", arguments[0])
	}
}

func generate(directory, name, password string) error {
	publicKey, privateKey, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	encrypted, err := minisign.EncryptKey(password, privateKey)
	if err != nil {
		return err
	}
	publicText, err := publicKey.MarshalText()
	if err != nil {
		return err
	}
	keyPath := filepath.Join(directory, name+".key")
	pubPath := filepath.Join(directory, name+".pub")
	for _, target := range []string{keyPath, pubPath} {
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("refusing to overwrite existing %s", target)
		}
	}
	if err := os.WriteFile(keyPath, encrypted, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, append(publicText, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s\nkey ID %016X\npublic key:\n%s\n", keyPath, pubPath, publicKey.ID(), publicText)
	return nil
}

func sign(keyPath, password string, files []string) error {
	privateKey, err := minisign.PrivateKeyFromFile(password, keyPath)
	if err != nil {
		return fmt.Errorf("open private key: %w", err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		trusted := fmt.Sprintf("timestamp:%d", time.Now().Unix())
		untrusted := "barn catalog: " + filepath.Base(file)
		signature := minisign.SignWithComments(privateKey, data, trusted, untrusted)
		if err := os.WriteFile(file+".minisig", signature, 0o644); err != nil {
			return err
		}
		fmt.Printf("signed %s -> %s.minisig\n", file, file)
	}
	return nil
}

func verify(pubPath string, files []string) error {
	publicKey, err := minisign.PublicKeyFromFile(pubPath)
	if err != nil {
		return err
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		signature, err := os.ReadFile(file + ".minisig")
		if err != nil {
			return err
		}
		if !minisign.Verify(publicKey, data, signature) {
			return fmt.Errorf("%s: signature verification FAILED", file)
		}
		fmt.Printf("verified %s\n", file)
	}
	return nil
}
