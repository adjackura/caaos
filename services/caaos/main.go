package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/google/shlex"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	metadataURL  = "http://metadata.google.internal/computeMetadata/v1/instance/attributes"
	metadataHang = "/?recursive=true&alt=json&wait_for_change=true&timeout_sec=120&last_etag="
	defaultEtag  = "NONE"
)

var (
	defaultTimeout = 130 * time.Second
	etag           = defaultEtag

	logger = log.New(os.Stdout, "[caaos]: ", log.LstdFlags|log.Lmicroseconds|log.Lshortfile)
)

type attributesJSON struct {
	ContainerID   string `json:"container-id"`
	ContainerArgs string `json:"container-args"`
	StopOnExit    bool   `json:"stop-on-exit,string"`
}

func runCmd(ctx context.Context, path string, args []string) error {
	logger.Printf("Running %q with args %q", path, args)

	c := exec.Command(path, args...)

	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pr.Close()

	c.Stdout = pw
	c.Stderr = pw

	if err := c.Start(); err != nil {
		return err
	}
	pw.Close()

	in := bufio.NewScanner(pr)
	for in.Scan() {
		logger.Printf("%s: %s", filepath.Base(path), in.Text())
	}

	return c.Wait()
}

func updateEtag(resp *http.Response) bool {
	oldEtag := etag
	etag = resp.Header.Get("etag")
	if etag == "" {
		etag = defaultEtag
	}
	return etag == oldEtag
}

func watchMetadata(ctx context.Context) (*attributesJSON, error) {
	client := &http.Client{
		Timeout: defaultTimeout,
	}

	req, err := http.NewRequest("GET", metadataURL+metadataHang+etag, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Metadata-Flavor", "Google")
	req = req.WithContext(ctx)

	for {
		resp, err := client.Do(req)
		// Don't return error on a canceled context.
		if err != nil && ctx.Err() != nil {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}

		// Only return metadata on updated etag.
		if updateEtag(resp) {
			continue
		}

		md, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		var attr attributesJSON
		return &attr, json.Unmarshal(md, &attr)
	}
}

func runContainer(ctx context.Context, client *containerd.Client, id string, args []string) error {
	logger.Println("pulling image")
	img, err := client.Pull(ctx, id, containerd.WithPullUnpack)
	if err != nil {
		return err
	}

	rnd := fmt.Sprintf("%d", time.Now().Unix())

	logger.Println("creating container")
	opts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithHostNamespace(specs.NetworkNamespace),
		oci.WithHostHostsFile,
		oci.WithHostResolvconf,
		//oci.WithTTY,
		oci.WithPrivileged,
		//oci.WithRootFSPath("/cntr"),
	}
	if len(args) > 0 {
		opts = append(opts, oci.WithProcessArgs(args...))
	}

	container, err := client.NewContainer(
		ctx,
		rnd,
		//containerd.WithImage(img),
		containerd.WithNewSnapshot(rnd, img),
		containerd.WithNewSpec(opts...),
	)
	if err != nil {
		return err
	}
	defer container.Delete(ctx, containerd.WithSnapshotCleanup)

	// create a new task
	logger.Println("creating task")
	task, err := container.NewTask(ctx, cio.NewCreator(append([]cio.Opt{cio.WithStdio})...))
	if err != nil {
		return err
	}

	pid := task.Pid()
	fmt.Println(pid)

	// Setup wait channel
	statusC, err := task.Wait(ctx)
	if err != nil {
		return err
	}

	// start the task
	logger.Println("running task")
	if err := task.Start(ctx); err != nil {
		return err
	}

	// wait for the task to exit and get the exit status
	logger.Println("waiting...")
	status := <-statusC
	code, _, err := status.Result()
	if err != nil {
		return err
	}

	logger.Println("return code:", code)

	logger.Println("deleting task")
	if _, err := task.Delete(ctx); err != nil {
		logger.Println(err)
	}

	// kill the process and get the exit status
	//if err := task.Kill(ctx, syscall.SIGTERM); err != nil {
	//	logger.Println(err)
	//}

	return nil
}

func main() {
	logger.Println("Starting caaos...")

	logger.Println("creating client")
	client, err := containerd.New("/run/containerd/containerd.sock")
	if err != nil {
		logger.Fatalln(err)
	}
	defer client.Close()

	ctx := namespaces.WithNamespace(context.Background(), "caaos")

	for {
		logger.Println("Waiting for metadata...")
		md, err := watchMetadata(ctx)
		if err != nil {
			logger.Println("Error grabing metadata:", err)
			time.Sleep(1 * time.Second)
			continue
		}

		if md.ContainerID == "" {
			logger.Println("No container set, waiting...")
			continue
		}

		var args []string
		if md.ContainerArgs != "" {
			args, err = shlex.Split(md.ContainerArgs)
			if err != nil {
				logger.Println("Error parsing arguments:", err)
				continue
			}
		}

		if err := runContainer(ctx, client, md.ContainerID, args); err != nil {
			logger.Println("Error:", err)
			time.Sleep(5 * time.Second)
		}

		if md.StopOnExit {
			logger.Printf("Finished running %s, shutting down", md.ContainerID)
			syscall.Sync()
			if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
				logger.Println("Error calling shutdown:", err)
			}
			select {}
		}

		logger.Printf("Finished running %s, waiting for next command...", md.ContainerID)
	}
}
