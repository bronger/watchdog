package main

import (
	"context"
	"io/fs"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

func isExcluded(path string, excludeRegexps []*regexp.Regexp) bool {
	for _, regexp := range excludeRegexps {
		if regexp.MatchString(path) {
			return true
		}
	}
	return false
}

type watchedDir struct {
	root              string
	agglomerationTime time.Duration
	workItems         chan workItem
	workPackages      chan []workItem
	watcher           *fsnotify.Watcher
	wg                sync.WaitGroup
	excludeRegexps    []*regexp.Regexp
}

func readConfiguration() (watchedDirs []watchedDir, currentDir string) {
	var configuration struct {
		CurrentDir  string `yaml:"current dir"`
		WatchedDirs []struct {
			Root              string
			AgglomerationTime string `yaml:"agglomeration ms"`
			Excludes          []string
		} `yaml:"watched dirs"`
	}
	data, err := ioutil.ReadFile(filepath.Join(os.Args[1], "configuration.yaml"))
	check(err)
	err = yaml.Unmarshal(data, &configuration)
	if err != nil || configuration.CurrentDir == "" || len(configuration.WatchedDirs) == 0 {
		logger.Panic("invalid configuration.yaml", err)
	}
	for _, configItem := range configuration.WatchedDirs {
		watchedDir := watchedDir{
			root:         configItem.Root,
			workItems:    make(chan workItem),
			workPackages: make(chan []workItem),
		}
		if configItem.AgglomerationTime == "" {
			watchedDir.agglomerationTime = 10 * time.Millisecond
		} else {
			ms, err := strconv.Atoi(configItem.AgglomerationTime)
			watchedDir.agglomerationTime = time.Duration(ms) * time.Millisecond
			check(err)
		}
		for _, pattern := range configItem.Excludes {
			excludeRegexp, err := regexp.Compile(pattern)
			check(err)
			watchedDir.excludeRegexps = append(watchedDir.excludeRegexps, excludeRegexp)
		}
		watchedDirs = append(watchedDirs, watchedDir)
	}
	return watchedDirs, configuration.CurrentDir
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

func addWatches(watcher *fsnotify.Watcher, root string) {
	err := filepath.WalkDir(root,
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

func eventsWatcher(ctx context.Context,
	watcher *fsnotify.Watcher, workItems chan<- workItem, excludeRegexps []*regexp.Regexp) {
	logger.Println("eventsWatcher: Starting")
	defer logger.Println("eventsWatcher: Shutting down")
	defer ctx.Value(wgKey).(*sync.WaitGroup).Done()
	for {
		select {
		case event := <-watcher.Events:
			if isExcluded(event.Name, excludeRegexps) {
				logger.Println("Ignored", event.Name)
				break
			}
			logger.Println(event)
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
		case <-ctx.Done():
			return
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

func workMarshaller(ctx context.Context,
	workItems <-chan workItem, workPackages chan<- []workItem, agglomerationTime time.Duration) {
	logger.Println("workMashaller: Starting")
	defer logger.Println("workMashaller: Shutting down")
	defer ctx.Value(wgKey).(*sync.WaitGroup).Done()
	defer close(workPackages)
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
					timer = time.NewTimer(agglomerationTime)
				case <-ctx.Done():
					return
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
					timer = time.NewTimer(agglomerationTime)
				case <-ctx.Done():
					return
				}
			}
		} else {
			select {
			case singleWorkItem := <-workItems:
				currentWorkItems = appendWorkItem(currentWorkItems, singleWorkItem)
				timer = time.NewTimer(agglomerationTime)
			case <-ctx.Done():
				return
			}
		}
	}
}

func worker(ctx context.Context, workPackages <-chan []workItem) {
	logger.Println("worker: Starting")
	defer logger.Println("worker: Shutting down")
	defer ctx.Value(wgKey).(*sync.WaitGroup).Done()
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
		err := cmd.Start()
		check(err)
		if err := waitOrStop(ctx, cmd, syscall.SIGTERM, 0); err != nil {
			logger.Println("External command error: ", err)
		}
		logger.Println("WORKER: Finished … waiting for new work")
	}
}

type key int

const wgKey key = 0

func main() {
	defer logger.Println("Exiting gracefully.")
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, wgKey, &wg)
	defer wg.Wait()
	go func() {
		<-sigs
		logger.Println("Received TERM")
		cancel()
	}()
	var err error
	watchedDirs, currentDir := readConfiguration()
	err = os.Chdir(currentDir)
	check(err)
	for _, watchedDir := range watchedDirs {
		wg.Add(3)
		go workMarshaller(ctx, watchedDir.workItems, watchedDir.workPackages, watchedDir.agglomerationTime)
		go worker(ctx, watchedDir.workPackages)
		watchedDir.watcher, err = fsnotify.NewWatcher()
		check(err)
		go eventsWatcher(ctx, watchedDir.watcher, watchedDir.workItems, watchedDir.excludeRegexps)
		logger.Println("Watching", watchedDir.root, "…")
		addWatches(watchedDir.watcher, watchedDir.root)
	}
}
