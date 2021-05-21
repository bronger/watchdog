package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

var logger *log.Logger

func check(e error) {
	if e != nil {
		logger.Panic(e)
	}
}

func init() {
	logger = log.New(os.Stderr, "", 0)
}

const (
	modified = iota
	deleted
)

type workItem struct {
	path      string
	isDir     bool
	eventType int
}

func longestPrefix(paths []string) string {
	if len(paths) == 0 {
		logger.Panic("longestPrefix: No paths given")
	}
	longest := strings.Split(paths[0], "/")
	for _, path := range paths[1:] {
		components := strings.Split(path, "/")
		var minimalLen int
		if len(longest) < len(components) {
			minimalLen = len(longest)
		} else {
			minimalLen = len(components)
		}
		longest = longest[:minimalLen]
		for i, component := range components[:minimalLen] {
			if component != longest[i] {
				longest = longest[:i]
			}
		}
		if len(longest) == 0 {
			break
		}
	}
	result := filepath.Join(longest...)
	if result == "" {
		result = "."
	}
	return result
}

func main() {
	watcher, err := fsnotify.NewWatcher()
	check(err)
	err = filepath.WalkDir(os.Args[2],
		func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				err = watcher.Add(path)
				check(err)
			}
			return nil
		})
	check(err)

	workItems := make(chan workItem)
	workPackages := make(chan []workItem)
	go func() {
		currentWorkItems := make([]workItem, 0, 100)
		for singleWorkItem := range workItems {
			currentWorkItems = append(currentWorkItems, singleWorkItem)
			select {
			case workPackages <- currentWorkItems:
				currentWorkItems = make([]workItem, 0, 100)
			default:
			}
		}
	}()

	scriptsDir := filepath.Dir(os.Args[1])

	go func() {
		for workPackage := range workPackages {
			var cmd *exec.Cmd
			if len(workPackage) > 1 {
				paths := make([]string, len(workPackage))
				for _, workItem := range workPackage {
					paths = append(paths, workItem.path)
				}
				cmd = exec.Command(filepath.Join(scriptsDir, "bulk_sync"), longestPrefix(paths))
			} else {
				workItem := workPackage[0]
				if workItem.isDir {
					cmd = exec.Command(filepath.Join(scriptsDir, "bulk_sync"), workItem.path)
				} else if workItem.eventType == modified {
					cmd = exec.Command(filepath.Join(scriptsDir, "copy"), workItem.path)
				} else {
					cmd = exec.Command(filepath.Join(scriptsDir, "delete"), workItem.path)
				}
			}
			if err := cmd.Run(); err != nil {
				logger.Println("External command error: ", err)
			}
		}
	}()

	logger.Println("Watching …")
	for {
		select {
		case event := <-watcher.Events:
			info, err := os.Stat(event.Name)
			if err != nil {
				logger.Println(err)
				break
			}
			fmt.Println(event.Name)
			var eventType int
			if event.Op&fsnotify.Create == fsnotify.Create ||
				event.Op&fsnotify.Write == fsnotify.Write ||
				event.Op&fsnotify.Chmod == fsnotify.Chmod {
				eventType = modified
			} else {
				eventType = deleted
			}
			workItems <- workItem{
				event.Name,
				info.IsDir(),
				eventType,
			}

		case err := <-watcher.Errors:
			check(err)
		}
	}
}
