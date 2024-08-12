package golang

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gptscript-ai/gptscript/pkg/credentials"
	"github.com/gptscript-ai/gptscript/pkg/debugcmd"
	runtimeEnv "github.com/gptscript-ai/gptscript/pkg/env"
	"github.com/gptscript-ai/gptscript/pkg/hash"
	"github.com/gptscript-ai/gptscript/pkg/repos/download"
	"github.com/gptscript-ai/gptscript/pkg/types"
)

//go:embed digests.txt
var releasesData []byte

const downloadURL = "https://go.dev/dl/"

type Runtime struct {
	// version something like "1.22.1"
	Version string
}

func (r *Runtime) ID() string {
	return "go" + r.Version
}

func (r *Runtime) GetHash(_ types.Tool) (string, error) {
	return "", nil
}

func (r *Runtime) Supports(tool types.Tool, cmd []string) bool {
	return tool.Source.IsGit() &&
		len(cmd) > 0 && cmd[0] == "${GPTSCRIPT_TOOL_DIR}/bin/gptscript-go-tool"
}

type release struct {
	account, repo, label string
}

func (r release) checksumTxt() string {
	return fmt.Sprintf(
		"https://github.com/%s/%s/releases/download/%s/checksums.txt",
		r.account,
		r.repo,
		r.label)
}

func (r release) binURL() string {
	return fmt.Sprintf(
		"https://github.com/%s/%s/releases/download/%s/%s",
		r.account,
		r.repo,
		r.label,
		r.srcBinName())
}

func (r release) targetBinName() string {
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}

	return "gptscript-go-tool" + suffix
}

func (r release) srcBinName() string {
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}

	return r.repo + "-" +
		runtime.GOOS + "-" +
		runtime.GOARCH + suffix
}

type tag struct {
	Name   string `json:"name,omitempty"`
	Commit struct {
		Sha string `json:"sha,omitempty"`
	} `json:"commit"`
}

func getLatestRelease(tool types.Tool) (*release, bool) {
	if tool.Source.Repo == nil || !strings.HasPrefix(tool.Source.Repo.Root, "https://github.com/") {
		return nil, false
	}

	parts := strings.Split(strings.TrimPrefix(strings.TrimSuffix(tool.Source.Repo.Root, ".git"), "https://"), "/")
	if len(parts) != 3 {
		return nil, false
	}

	client := http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	account, repo := parts[1], parts[2]

	resp, err := client.Get(fmt.Sprintf("https://api.github.com/repos/%s/%s/tags", account, repo))
	if err != nil || resp.StatusCode != http.StatusOK {
		// ignore error
		return nil, false
	}
	defer resp.Body.Close()

	var tags []tag
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, false
	}
	for _, tag := range tags {
		if tag.Commit.Sha == tool.Source.Repo.Revision {
			return &release{
				account: account,
				repo:    repo,
				label:   tag.Name,
			}, true
		}
	}

	resp, err = client.Get(fmt.Sprintf("https://github.com/%s/%s/releases/latest", account, repo))
	if err != nil || resp.StatusCode != http.StatusFound {
		// ignore error
		return nil, false
	}
	defer resp.Body.Close()

	target := resp.Header.Get("Location")
	if target == "" {
		return nil, false
	}

	parts = strings.Split(target, "/")
	label := parts[len(parts)-1]

	return &release{
		account: account,
		repo:    repo,
		label:   label,
	}, true
}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	} else if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("bad HTTP status code: %d", resp.StatusCode)
	}

	return resp, nil
}

func downloadBin(ctx context.Context, checksum, src, url, bin string) error {
	resp, err := get(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := os.MkdirAll(filepath.Join(src, "bin"), 0755); err != nil {
		return err
	}

	targetFile, err := os.Create(filepath.Join(src, "bin", bin))
	if err != nil {
		return err
	}

	digest := sha256.New()

	if _, err := io.Copy(io.MultiWriter(targetFile, digest), resp.Body); err != nil {
		return err
	}

	if err := targetFile.Close(); err != nil {
		return nil
	}

	if got := hex.EncodeToString(digest.Sum(nil)); got != checksum {
		return fmt.Errorf("checksum mismatch %s != %s", got, checksum)
	}

	if err := os.Chmod(targetFile.Name(), 0755); err != nil {
		return err
	}

	return nil
}

func getChecksum(ctx context.Context, rel *release) string {
	resp, err := get(ctx, rel.checksumTxt())
	if err != nil {
		// ignore error
		return ""
	}
	defer resp.Body.Close()

	scan := bufio.NewScanner(resp.Body)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) != 2 || fields[1] != rel.srcBinName() {
			continue
		}
		return fields[0]
	}

	return ""
}

func (r *Runtime) Binary(ctx context.Context, tool types.Tool, _, toolSource string, env []string) (bool, []string, error) {
	if !tool.Source.IsGit() {
		return false, nil, nil
	}

	rel, ok := getLatestRelease(tool)
	if !ok {
		return false, nil, nil
	}

	checksum := getChecksum(ctx, rel)
	if checksum == "" {
		return false, nil, nil
	}

	if err := downloadBin(ctx, checksum, toolSource, rel.binURL(), rel.targetBinName()); err != nil {
		// ignore error
		return false, nil, nil
	}

	return true, env, nil
}

func (r *Runtime) Setup(ctx context.Context, _ types.Tool, dataRoot, toolSource string, env []string) ([]string, error) {
	binPath, err := r.getRuntime(ctx, dataRoot)
	if err != nil {
		return nil, err
	}

	newEnv := runtimeEnv.AppendPath(env, binPath)
	if err := r.runBuild(ctx, toolSource, binPath, append(env, newEnv...)); err != nil {
		return nil, err
	}

	return newEnv, nil
}

func (r *Runtime) BuildCredentialHelper(ctx context.Context, helperName string, credHelperDirs credentials.CredentialHelperDirs, dataRoot, revision string, env []string) error {
	if helperName == "file" {
		return nil
	}

	var suffix string
	if helperName == "wincred" {
		suffix = ".exe"
	}

	binPath, err := r.getRuntime(ctx, dataRoot)
	if err != nil {
		return err
	}
	newEnv := runtimeEnv.AppendPath(env, binPath)

	log.InfofCtx(ctx, "Building credential helper %s", helperName)
	cmd := debugcmd.New(ctx, filepath.Join(binPath, "go"),
		"build", "-buildvcs=false", "-o",
		filepath.Join(credHelperDirs.BinDir, "gptscript-credential-"+helperName+suffix),
		fmt.Sprintf("./%s/cmd/", helperName))
	cmd.Env = stripGo(append(env, newEnv...))
	cmd.Dir = filepath.Join(credHelperDirs.RepoDir, revision)
	return cmd.Run()
}

func (r *Runtime) getReleaseAndDigest() (string, string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(releasesData))
	key := r.ID() + "." + runtime.GOOS + "-" + runtime.GOARCH
	for scanner.Scan() {
		line := strings.Split(scanner.Text(), "  ")
		file, digest := strings.TrimSpace(line[1]), strings.TrimSpace(line[0])
		if strings.HasPrefix(file, key) {
			return downloadURL + file, digest, nil
		}
	}

	return "", "", fmt.Errorf("failed to find %s release for os=%s arch=%s", r.ID(), runtime.GOOS, runtime.GOARCH)
}

func stripGo(env []string) (result []string) {
	for _, env := range env {
		if strings.HasPrefix(env, "GO") {
			continue
		}
		result = append(result, env)
	}
	return
}

func (r *Runtime) runBuild(ctx context.Context, toolSource, binDir string, env []string) error {
	log.InfofCtx(ctx, "Running go build in %s", toolSource)
	cmd := debugcmd.New(ctx, filepath.Join(binDir, "go"), "build", "-buildvcs=false", "-o", artifactName())
	cmd.Env = stripGo(env)
	cmd.Dir = toolSource
	return cmd.Run()
}

func artifactName() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("bin", "gptscript-go-tool.exe")
	}
	return filepath.Join("bin", "gptscript-go-tool")
}

func (r *Runtime) binDir(rel string) string {
	return filepath.Join(rel, "go", "bin")
}

func (r *Runtime) getRuntime(ctx context.Context, cwd string) (string, error) {
	url, sha, err := r.getReleaseAndDigest()
	if err != nil {
		return "", err
	}

	target := filepath.Join(cwd, "golang", hash.ID(url, sha))
	if _, err := os.Stat(target); err == nil {
		return r.binDir(target), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	log.InfofCtx(ctx, "Downloading Go %s", r.Version)
	tmp := target + ".download"
	defer os.RemoveAll(tmp)

	if err := os.MkdirAll(tmp, 0755); err != nil {
		return "", err
	}

	if err := download.Extract(ctx, url, sha, tmp); err != nil {
		return "", err
	}

	if err := os.Rename(tmp, target); err != nil {
		return "", err
	}

	return r.binDir(target), nil
}
