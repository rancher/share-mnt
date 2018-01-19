package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/codegangsta/cli"
	"github.com/docker/docker/pkg/mount"
)

const (
	version = "0.0.0"
)

var (
	cgroupPattern = regexp.MustCompile("^.*/docker-([a-z0-9]+).scope$")
	// Add the statepath as found on most OS's, and prefix with '/var' for Boot2Docker
	statePaths = []string{
		"/run/runc",
		"/var/run/runc",
		"/run/docker/execdriver/native",
		"/var/run/docker/execdriver/native",
		"/run/docker/runtime-runc/moby",
		"/var/run/docker/runtime-runc/moby",
	}
)

func main() {
	app := cli.NewApp()
	app.Version = version
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name: "stage2",
		},
	}
	app.Action = func(cli *cli.Context) {
		var fun cliFunc

		if cli.GlobalBool("stage2") {
			fun = stage2
		} else {
			fun = start
		}

		i, err := fun(cli)
		if err != nil {
			logrus.Fatal(err)
		}
		os.Exit(i)
	}
	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

type cliFunc func(cli *cli.Context) (int, error)

type State struct {
	InitProcessPid int    `json:"init_process_pid"`
	Config         Config `json:"config"`
}

type Config struct {
	Rootfs string `json:"rootfs"`
}

func stage2(cli *cli.Context) (int, error) {
	for _, val := range cli.Args() {
		if val == "--" {
			break
		}

		if _, err := os.Stat(val); os.IsNotExist(err) {
			if err := os.MkdirAll(val, 0755); err != nil {
				return -1, err
			}
		}

		if err := mount.MakeShared(val); err != nil {
			logrus.Errorf("Failed to make shared %s: %v", val, err)
			return -1, err
		}
	}

	return 0, nil
}

func start(cli *cli.Context) (int, error) {
	paths := []string{}
	for _, i := range statePaths {
		paths = append(paths, filepath.Join("/host", i))
	}
	state, err := findState(paths...)
	if err != nil {
		return -1, err
	}

	mnt, err := getMntFd(state.InitProcessPid)
	if err != nil {
		return -1, err
	}

	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		return -1, err
	}

	nsenter, err := exec.LookPath("nsenter")
	if err != nil {
		logrus.Error("Failed to find nsenter:", err)
		return -1, err
	}

	args := []string{nsenter, "--mount=" + mnt, "-F", "--", path.Join(state.Config.Rootfs, self), "--stage2"}
	args = append(args, os.Args[1:]...)

	logrus.Infof("Execing %v", args)
	return -1, syscall.Exec(nsenter, args, os.Environ())
}

func getMntFd(pid int) (string, error) {
	psStat := fmt.Sprintf("/proc/%d/stat", pid)
	content, err := ioutil.ReadFile(psStat)
	if err != nil {
		return "", err
	}

	ppid := strings.Split(strings.SplitN(string(content), ")", 2)[1], " ")[2]
	return fmt.Sprintf("/proc/%s/ns/mnt", ppid), nil
}

func findContainerID() (string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/cgroup", os.Getpid()))
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "docker/") && strings.Contains(line, ":devices:") {
			parts := strings.Split(line, "/")
			return parts[len(parts)-1], nil
		}
	}

	f.Seek(0, 0)
	scanner = bufio.NewScanner(f)
	for scanner.Scan() {
		matches := cgroupPattern.FindAllStringSubmatch(scanner.Text(), -1)
		if len(matches) > 0 && len(matches[0]) > 1 && matches[0][1] != "" {
			return matches[0][1], nil
		}
	}

	content, _ := ioutil.ReadFile(fmt.Sprintf("/proc/%d/cgroup", os.Getpid()))
	return "", fmt.Errorf("Failed to find container id:\n%s", string(content))
}

func findState(stateRoots ...string) (*State, error) {
	containerID, err := findContainerID()
	if err != nil {
		return nil, err
	}
	fmt.Println("Found container ID:", containerID)

	for _, stateRoot := range stateRoots {
		fmt.Println("Checking root:", stateRoot)
		files, err := ioutil.ReadDir(stateRoot)
		if err != nil {
			continue
		}

		for _, file := range files {
			fmt.Println("Checking file:", file.Name())
			if !strings.HasPrefix(file.Name(), containerID) {
				continue
			}

			bytes, err := ioutil.ReadFile(path.Join(stateRoot, file.Name(), "state.json"))
			if err != nil {
				continue
			}

			fmt.Println("Found state.json:", file.Name())
			var state State
			return &state, json.Unmarshal(bytes, &state)
		}
	}

	return nil, errors.New("Failed to find state.json")
}
