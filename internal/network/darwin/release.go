// Package darwin contains the pinned socket_vmnet supply-chain and install
// plan for macOS private networking. Privileged execution is kept separate.
package darwin

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const ReleaseVersion = "1.2.2"

type Release struct {
	Version      string
	Arch         string
	ArchiveName  string
	URL          string
	SHA256       string
	SocketSHA256 string
	ClientSHA256 string
}

var releases = map[string]Release{
	"arm64": {
		Version: ReleaseVersion, Arch: "arm64",
		ArchiveName:  "socket_vmnet-1.2.2-arm64.tar.gz",
		URL:          "https://github.com/lima-vm/socket_vmnet/releases/download/v1.2.2/socket_vmnet-1.2.2-arm64.tar.gz",
		SHA256:       "c7bf62308fbcfdc29bdfb8373c9b1951f7ac2396446e4390919796a94972e6dc",
		SocketSHA256: "b8a72a62237312f2f756027dea504a844edeb40014702d4a320292c026d282b0",
		ClientSHA256: "2d2e364b808f0b43a92bdd7ac3c7b390d6b55a33c761c61c0738d688286a3eff",
	},
	"amd64": {
		Version: ReleaseVersion, Arch: "amd64",
		ArchiveName:  "socket_vmnet-1.2.2-x86_64.tar.gz",
		URL:          "https://github.com/lima-vm/socket_vmnet/releases/download/v1.2.2/socket_vmnet-1.2.2-x86_64.tar.gz",
		SHA256:       "2968a82c97e692c2d36f87230152e8018e00589c1b598e8257775adfe83800a1",
		SocketSHA256: "526086c810df74a2465da6fd841a6e7a9a6a74679029dc76eee9dee2291b4929",
		ClientSHA256: "c176dd45930bb21229c6423ea8bb27e200f503b8d62caa39af1057a8f5dec2d9",
	},
}

func PinnedRelease(arch string) (Release, error) {
	release, ok := releases[arch]
	if !ok {
		return Release{}, fmt.Errorf("socket_vmnet v%s has no pinned artifact for architecture %q", ReleaseVersion, arch)
	}
	return release, nil
}

func fileDigest(path string) (string, error) {
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fileInfo.Mode().IsRegular() {
		return "", errors.New("socket_vmnet archive must be a regular non-symlink file")
	}
	handle, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		// The archive handle is read-only; its bounded digest is already held
		// by the caller before this descriptor is released.
		_ = handle.Close()
	}()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(handle, 4<<20)); err != nil {
		return "", err
	}
	info, err := handle.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > 4<<20 {
		return "", errors.New("socket_vmnet archive exceeds 4 MiB safety limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func canonicalArchivePath(path string) (string, error) {
	if path == "" {
		return "", errors.New("socket_vmnet archive path is empty")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("socket_vmnet archive must be a regular file")
	}
	return canonical, nil
}

type ArchiveInfo struct {
	SocketSize int64
	ClientSize int64
}

func safeArchiveName(name string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if clean == "." {
		return clean, nil
	}
	if strings.HasPrefix(name, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	if clean != "opt" && clean != "opt/socket_vmnet" && !strings.HasPrefix(clean, "opt/socket_vmnet/") {
		return "", fmt.Errorf("archive path outside opt/socket_vmnet: %q", name)
	}
	return clean, nil
}

func inspectArchive(reader io.Reader) (ArchiveInfo, error) {
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return ArchiveInfo{}, fmt.Errorf("open socket_vmnet gzip: %w", err)
	}
	defer func() {
		// Full archive consumption performs the format/checksum checks; Close
		// only releases read-side gzip state.
		_ = gzipReader.Close()
	}()
	tarReader := tar.NewReader(gzipReader)
	var info ArchiveInfo
	seenSocket, seenClient := false, false
	for entries := 0; ; entries++ {
		if entries > 64 {
			return ArchiveInfo{}, errors.New("socket_vmnet archive has too many entries")
		}
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ArchiveInfo{}, fmt.Errorf("read socket_vmnet tar: %w", err)
		}
		clean, err := safeArchiveName(header.Name)
		if err != nil {
			return ArchiveInfo{}, err
		}
		if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg {
			return ArchiveInfo{}, fmt.Errorf("socket_vmnet archive contains unsupported entry type for %q", header.Name)
		}
		if header.Size < 0 || header.Size > 1<<20 {
			return ArchiveInfo{}, fmt.Errorf("socket_vmnet archive entry %q has unsafe size %d", header.Name, header.Size)
		}
		switch clean {
		case "opt/socket_vmnet/bin/socket_vmnet":
			if seenSocket {
				return ArchiveInfo{}, errors.New("socket_vmnet executable appears more than once")
			}
			if header.Typeflag != tar.TypeReg {
				return ArchiveInfo{}, errors.New("socket_vmnet executable is not a regular file")
			}
			seenSocket, info.SocketSize = true, header.Size
		case "opt/socket_vmnet/bin/socket_vmnet_client":
			if seenClient {
				return ArchiveInfo{}, errors.New("socket_vmnet_client executable appears more than once")
			}
			if header.Typeflag != tar.TypeReg {
				return ArchiveInfo{}, errors.New("socket_vmnet_client executable is not a regular file")
			}
			seenClient, info.ClientSize = true, header.Size
		}
	}
	if !seenSocket || !seenClient || info.SocketSize == 0 || info.ClientSize == 0 {
		return ArchiveInfo{}, errors.New("socket_vmnet archive is missing required executables")
	}
	return info, nil
}

// VerifyArchive uses the embedded per-architecture digest before inspecting
// any tar metadata. The upstream SHA256SUMS file is not consulted.
func VerifyArchive(archivePath, arch string) (ArchiveInfo, error) {
	canonical, err := canonicalArchivePath(archivePath)
	if err != nil {
		return ArchiveInfo{}, err
	}
	return verifyArchiveFile(canonical, arch)
}

func verifyArchiveFile(archivePath, arch string) (ArchiveInfo, error) {
	release, err := PinnedRelease(arch)
	if err != nil {
		return ArchiveInfo{}, err
	}
	digest, err := fileDigest(archivePath)
	if err != nil {
		return ArchiveInfo{}, err
	}
	if digest != release.SHA256 {
		return ArchiveInfo{}, fmt.Errorf("socket_vmnet archive digest %s does not match embedded %s", digest, release.SHA256)
	}
	handle, err := os.Open(archivePath)
	if err != nil {
		return ArchiveInfo{}, err
	}
	defer func() {
		// Archive inspection is read-only and consumes the bounded stream before
		// returning its verified inventory.
		_ = handle.Close()
	}()
	return inspectArchive(handle)
}

func extractOne(archivePath, targetName, targetPath, expectedDigest string) error {
	handle, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() {
		// Extraction reads from a digest-verified archive; target publication
		// handles its own Write, Sync, and Close failures.
		_ = handle.Close()
	}()
	gzipReader, err := gzip.NewReader(handle)
	if err != nil {
		return err
	}
	defer func() {
		// Full archive consumption performs the format/checksum checks; Close
		// only releases read-side gzip state.
		_ = gzipReader.Close()
	}()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			return fmt.Errorf("archive entry %s not found", targetName)
		}
		if nextErr != nil {
			return nextErr
		}
		clean, cleanErr := safeArchiveName(header.Name)
		if cleanErr != nil {
			return cleanErr
		}
		if clean != targetName {
			continue
		}
		output, openErr := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
		if openErr != nil {
			return openErr
		}
		keep := false
		defer func() {
			if !keep {
				_ = os.Remove(targetPath)
			}
		}()
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(tarReader, 1<<20))
		if copyErr == nil {
			copyErr = output.Sync()
		}
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		if expectedDigest != "" && digest != expectedDigest {
			return fmt.Errorf("extracted %s digest %s does not match embedded %s", targetName, digest, expectedDigest)
		}
		keep = true
		return nil
	}
}

// LocalBinaries names prebuilt socket_vmnet executables already on this host,
// such as the keg of a version-matched Homebrew formula.
type LocalBinaries struct {
	Socket string `json:"socket"`
	Client string `json:"client"`
}

func localBinaryDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 4<<20 {
		return "", fmt.Errorf("socket_vmnet binary must be a regular non-empty file under 4 MiB: %s", path)
	}
	digest, err := fileDigest(path)
	if err != nil {
		return "", fmt.Errorf("digest %s: %w", path, err)
	}
	return digest, nil
}

// VerifyLocalBinaries digests prebuilt executables so the install plan can
// record them (trust-on-first-use at the moment root copies them; every later
// verification pins against the recorded digests).
func VerifyLocalBinaries(binaries LocalBinaries) (socketSHA, clientSHA string, err error) {
	if !filepath.IsAbs(binaries.Socket) || !filepath.IsAbs(binaries.Client) {
		return "", "", errors.New("socket_vmnet local binary paths must be absolute")
	}
	if socketSHA, err = localBinaryDigest(binaries.Socket); err != nil {
		return "", "", err
	}
	if clientSHA, err = localBinaryDigest(binaries.Client); err != nil {
		return "", "", err
	}
	return socketSHA, clientSHA, nil
}

func emptyStagingDir(stagingDir string) error {
	if stagingDir == "" || !filepath.IsAbs(stagingDir) {
		return errors.New("socket_vmnet staging directory must be absolute")
	}
	if err := os.Mkdir(stagingDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(stagingDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("socket_vmnet staging path must be a real directory")
	}
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("socket_vmnet staging directory must be empty")
	}
	return nil
}

func stageOne(sourcePath, targetPath, expectedDigest string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer func() {
		// The source archive is read-only and its copied bytes are verified
		// before publication.
		_ = source.Close()
	}()
	output, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(targetPath)
		}
	}()
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(source, 4<<20))
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if digest := hex.EncodeToString(hash.Sum(nil)); digest != expectedDigest {
		return fmt.Errorf("staged %s digest %s does not match recorded %s", filepath.Base(targetPath), digest, expectedDigest)
	}
	keep = true
	return nil
}

// StageVerifiedLocalBinaries copies prebuilt executables into a new or empty
// user-owned staging directory and requires the staged bytes to match the
// digests already recorded in the install plan, guarding against the source
// files changing between planning and staging.
func StageVerifiedLocalBinaries(binaries LocalBinaries, stagingDir, socketSHA, clientSHA string) error {
	if !hexDigestPattern.MatchString(socketSHA) || !hexDigestPattern.MatchString(clientSHA) {
		return errors.New("socket_vmnet staging requires recorded binary digests")
	}
	if err := emptyStagingDir(stagingDir); err != nil {
		return err
	}
	if err := stageOne(binaries.Socket, filepath.Join(stagingDir, "socket_vmnet"), socketSHA); err != nil {
		return err
	}
	return stageOne(binaries.Client, filepath.Join(stagingDir, "socket_vmnet_client"), clientSHA)
}

// ExtractVerifiedBinaries writes only the two executables into a new or empty
// user-owned staging directory. Privileged code must install and re-verify the
// bytes in a root-owned destination before launching them.
func ExtractVerifiedBinaries(archivePath, arch, stagingDir string) error {
	canonical, err := canonicalArchivePath(archivePath)
	if err != nil {
		return err
	}
	if _, err := verifyArchiveFile(canonical, arch); err != nil {
		return err
	}
	if err := emptyStagingDir(stagingDir); err != nil {
		return err
	}
	release, _ := PinnedRelease(arch)
	if err := extractOne(canonical, "opt/socket_vmnet/bin/socket_vmnet", filepath.Join(stagingDir, "socket_vmnet"), release.SocketSHA256); err != nil {
		return err
	}
	if err := extractOne(canonical, "opt/socket_vmnet/bin/socket_vmnet_client", filepath.Join(stagingDir, "socket_vmnet_client"), release.ClientSHA256); err != nil {
		return err
	}
	return nil
}
