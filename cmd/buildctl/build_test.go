package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/continuity/fs/fstest"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/util/testutil/integration"
	"github.com/stretchr/testify/require"
)

func testBuildWithLocalFiles(t *testing.T, sb integration.Sandbox) {
	dir, err := tmpdir(
		fstest.CreateFile("foo", []byte("bar"), 0600),
	)
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	st := llb.Image("busybox").
		Run(llb.Shlex("sh -c 'echo -n bar > foo2'")).
		Run(llb.Shlex("cmp -s /mnt/foo foo2"))

	st.AddMount("/mnt", llb.Local("src"), llb.Readonly)

	rdr, err := marshal(sb.Context(), st.Root())
	require.NoError(t, err)

	cmd := sb.Cmd(fmt.Sprintf("build --progress=plain --local src=%s", dir))
	cmd.Stdin = rdr

	err = cmd.Run()
	require.NoError(t, err)
}

func testBuildLocalExporter(t *testing.T, sb integration.Sandbox) {
	st := llb.Image("busybox").
		Run(llb.Shlex("sh -c 'echo -n bar > /out/foo'"))

	out := st.AddMount("/out", llb.Scratch())

	rdr, err := marshal(sb.Context(), out)
	require.NoError(t, err)

	tmpdir, err := ioutil.TempDir("", "buildkit-buildctl")
	require.NoError(t, err)
	defer os.RemoveAll(tmpdir)

	cmd := sb.Cmd(fmt.Sprintf("build --progress=plain --exporter=local --exporter-opt output=%s", tmpdir))
	cmd.Stdin = rdr
	err = cmd.Run()

	require.NoError(t, err)

	dt, err := ioutil.ReadFile(filepath.Join(tmpdir, "foo"))
	require.NoError(t, err)
	require.Equal(t, string(dt), "bar")
}

func testBuildContainerdExporter(t *testing.T, sb integration.Sandbox) {
	cdAddress := sb.ContainerdAddress()
	if cdAddress == "" {
		t.Skip("test is only for containerd worker")
	}

	st := llb.Image("busybox").
		Run(llb.Shlex("sh -c 'echo -n bar > /foo'"))

	rdr, err := marshal(sb.Context(), st.Root())
	require.NoError(t, err)

	imageName := "example.com/moby/imageexporter:test"

	buildCmd := []string{
		"build", "--progress=plain",
		"--exporter=image", "--exporter-opt", "unpack=true",
		"--exporter-opt", "name=" + imageName,
	}

	cmd := sb.Cmd(strings.Join(buildCmd, " "))
	cmd.Stdin = rdr
	err = cmd.Run()
	require.NoError(t, err)

	client, err := containerd.New(cdAddress, containerd.WithTimeout(60*time.Second))
	require.NoError(t, err)
	defer client.Close()

	ctx := namespaces.WithNamespace(context.Background(), "buildkit")

	img, err := client.GetImage(ctx, imageName)
	require.NoError(t, err)

	// NOTE: by default, it is overlayfs
	snapshotter := "overlayfs"
	if sn := sb.Snapshotter(); sn != "" {
		snapshotter = sn
	}
	ok, err := img.IsUnpacked(ctx, snapshotter)
	require.NoError(t, err)
	require.Equal(t, ok, true)
}

func testBuildMetadataFile(t *testing.T, sb integration.Sandbox) {
	st := llb.Image("busybox").
		Run(llb.Shlex("sh -c 'echo -n bar > /foo'"))

	rdr, err := marshal(sb.Context(), st.Root())
	require.NoError(t, err)

	tmpDir, err := ioutil.TempDir("", "buildkit-buildctl")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	imageName := "example.com/moby/metadata:test"
	metadataFile := filepath.Join(tmpDir, "metadata.json")

	buildCmd := []string{
		"build", "--progress=plain",
		"--output type=image,name=" + imageName + ",push=false",
		"--metadata-file", metadataFile,
	}

	cmd := sb.Cmd(strings.Join(buildCmd, " "))
	cmd.Stdin = rdr
	err = cmd.Run()
	require.NoError(t, err)

	require.FileExists(t, metadataFile)
	metadataBytes, err := ioutil.ReadFile(metadataFile)
	require.NoError(t, err)

	var metadata map[string]string
	err = json.Unmarshal(metadataBytes, &metadata)
	require.NoError(t, err)

	require.Equal(t, imageName, metadata["image.name"])

	digest := metadata["containerimage.digest"]
	require.NotEmpty(t, digest)

	cdAddress := sb.ContainerdAddress()
	if cdAddress == "" {
		t.Log("no containerd worker, skipping digest verification")
	} else {
		client, err := containerd.New(cdAddress, containerd.WithTimeout(60*time.Second))
		require.NoError(t, err)
		defer client.Close()

		ctx := namespaces.WithNamespace(context.Background(), "buildkit")

		img, err := client.GetImage(ctx, imageName)
		require.NoError(t, err)

		require.Equal(t, img.Metadata().Target.Digest.String(), digest)
	}
}

func marshal(ctx context.Context, st llb.State) (io.Reader, error) {
	def, err := st.Marshal(ctx)
	if err != nil {
		return nil, err
	}
	dt, err := def.ToPB().Marshal()
	if err != nil {
		return nil, err
	}
	return bytes.NewBuffer(dt), nil
}

func tmpdir(appliers ...fstest.Applier) (string, error) {
	tmpdir, err := ioutil.TempDir("", "buildkit-buildctl")
	if err != nil {
		return "", err
	}
	if err := fstest.Apply(appliers...).Apply(tmpdir); err != nil {
		return "", err
	}
	return tmpdir, nil
}
