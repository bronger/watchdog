package main

import (
	"fmt"
	"io/fs"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v2"
)

var logger *log.Logger

func check(e error) {
	if e != nil {
		logger.Panic(e)
	}
}

func isExcluded(path string) bool {
	for _, regexp := range watchedDirs[0].excludeRegexps {
		if regexp.MatchString(path) {
			return true
		}
	}
	return false
}

type watchedDir struct {
	root           string
	workItems      chan workItem
	workPackages   chan []workItem
	watcher        *fsnotify.Watcher
	wg             sync.WaitGroup
	excludeRegexps []*regexp.Regexp
}

var watchedDirs []watchedDir

func readConfiguration() {
	var configuration []struct {
		Excludes []string
	}
	data, err := ioutil.ReadFile(filepath.Join(os.Args[1], "configuration.yaml"))
	check(err)
	err = yaml.Unmarshal(data, &configuration)
	if err != nil {
		logger.Panic("invalid configuration.yaml", err)
	}
	watchedDir := watchedDir{
		root: os.Args[2],
	}
	for _, pattern := range configuration[0].Excludes {
		excludeRegexp, err := regexp.Compile(pattern)
		check(err)
		watchedDir.excludeRegexps = append(watchedDir.excludeRegexps, excludeRegexp)
	}
	watchedDirs = append(watchedDirs, watchedDir)
}

func init() {
	logger = log.New(os.Stderr, "", 0)
	readConfiguration()
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

func addWatches(watcher *fsnotify.Watcher) {
	err := filepath.WalkDir(watchedDirs[0].root,
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
}

func eventsWatcher(watcher *fsnotify.Watcher, workItems chan<- workItem) {
	for {
		select {
		case event := <-watcher.Events:
			if isExcluded(event.Name) {
				logger.Println("Ignored", event.Name)
				break
			}
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

func appendWorkItem(workItems []workItem, workItem workItem) []workItem {
	for i := range workItems {
		i = len(workItems) - 1 - i
		item := workItems[i]
		if item == workItem {
			logger.Println("appendWorkItem: Ignored duplicate")
			return workItems
		}
		if item.path == workItem.path && workItem.eventType == deleted && item.eventType == modified {
			logger.Println("appendWorkItem: \"modified\" replaced with \"deleted\"")
			workItems[i] = workItem
			return workItems
		}
	}
	logger.Println("appendWorkItem: Appended new work item", workItem)
	return append(workItems, workItem)
}

func workMarshaller(workItems <-chan workItem, workPackages chan<- []workItem) {
	currentWorkItems := make([]workItem, 0, 100)
	var timer *time.Timer
	for {
		if len(currentWorkItems) > 0 {
			if timer == nil {
				select {
				case workPackages <- currentWorkItems:
					currentWorkItems = make([]workItem, 0, 100)
				case singleWorkItem := <-workItems:
					currentWorkItems = appendWorkItem(currentWorkItems, singleWorkItem)
					timer = time.NewTimer(10 * time.Millisecond)
				}
			} else {
				select {
				case <-timer.C:
					timer = nil
					select {
					case workPackages <- currentWorkItems:
						currentWorkItems = make([]workItem, 0, 100)
					default:
					}
				case singleWorkItem := <-workItems:
					currentWorkItems = appendWorkItem(currentWorkItems, singleWorkItem)
					timer = time.NewTimer(10 * time.Millisecond)
				}
			}
		} else {
			currentWorkItems = appendWorkItem(currentWorkItems, <-workItems)
			timer = time.NewTimer(10 * time.Millisecond)
		}
	}
}

func worker(workPackages <-chan []workItem) {
	scriptsDir := os.Args[1]
	for workPackage := range workPackages {
		logger.Println("WORKER: New work!")
		var cmd *exec.Cmd
		if len(workPackage) > 1 {
			paths := make([]string, 0, len(workPackage))
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
	workItems := make(chan workItem)
	workPackages := make(chan []workItem)
	go workMarshaller(workItems, workPackages)
	go worker(workPackages)
	watcher, err := fsnotify.NewWatcher()
	check(err)
	done := make(chan bool)
	go eventsWatcher(watcher, workItems)
	logger.Println("Watching", watchedDirs[0].root, "…")
	addWatches(watcher)
	<-done
}
