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
const (
	unknown = iota
	directory
	file
)

type workItem struct {
	path      string
	nodeType  int
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

func getWatcher() *fsnotify.Watcher {
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
	return watcher
}

func eventsWatcher(watcher *fsnotify.Watcher, workItems chan<- workItem) {
	for {
		select {
		case event := <-watcher.Events:
			fmt.Println(event)
			newWorkItem := workItem{path: event.Name}
			if event.Op&fsnotify.Create == fsnotify.Create ||
				event.Op&fsnotify.Write == fsnotify.Write ||
				event.Op&fsnotify.Chmod == fsnotify.Chmod {
				newWorkItem.eventType = modified
				info, err := os.Stat(event.Name)
				if err != nil {
					logger.Println(err)
					newWorkItem.nodeType = unknown
				} else if info.IsDir() {
					newWorkItem.nodeType = directory
				} else {
					newWorkItem.nodeType = file
				}
			} else {
				newWorkItem.eventType = deleted
			}
			workItems <- newWorkItem

		case err := <-watcher.Errors:
			check(err)
		}
	}
}

func workMarshaller(workItems <-chan workItem, workPackages chan<- []workItem) {
	currentWorkItems := make([]workItem, 0, 100)
	for {
		if len(currentWorkItems) > 0 {
			select {
			case singleWorkItem := <-workItems:
				currentWorkItems = append(currentWorkItems, singleWorkItem)
			case workPackages <- currentWorkItems:
				currentWorkItems = make([]workItem, 0, 100)
			}
		} else {
			currentWorkItems = append(currentWorkItems, <-workItems)
		}
	}
}

func worker(workPackages <-chan []workItem) {
	scriptsDir := os.Args[1]
	for workPackage := range workPackages {
		logger.Println("WORKER: New work!")
		var cmd *exec.Cmd
		if len(workPackage) > 1 {
			paths := make([]string, len(workPackage))
			for _, workItem := range workPackage {
				paths = append(paths, workItem.path)
			}
			logger.Println("Calling bulk_sync due to more than one change")
			cmd = exec.Command(filepath.Join(scriptsDir, "bulk_sync"), longestPrefix(paths))
		} else {
			workItem := workPackage[0]
			if workItem.eventType == deleted {
				logger.Println("Calling delete")
				cmd = exec.Command(filepath.Join(scriptsDir, "delete"), workItem.path)
			} else if workItem.nodeType == file {
				logger.Println("Calling copy")
				cmd = exec.Command(filepath.Join(scriptsDir, "copy"), workItem.path)
			} else {
				logger.Println("Calling build_sync because non-file was changed")
				cmd = exec.Command(filepath.Join(scriptsDir, "bulk_sync"), workItem.path)
			}
		}
		if err := cmd.Run(); err != nil {
			logger.Println("External command error: ", err)
		}
		logger.Println("WORKER: Finished … waiting for new work")
	}
}

func main() {
	watcher := getWatcher()
	workItems := make(chan workItem)
	workPackages := make(chan []workItem)
	go workMarshaller(workItems, workPackages)
	go worker(workPackages)
	logger.Println("Watching …")
	eventsWatcher(watcher, workItems)
}
