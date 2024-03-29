package main

import (
	"context"
	"io/fs"
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
	configurationFilePath := filepath.Join(os.Args[1], "configuration.yaml")
	if data, err := os.ReadFile(configurationFilePath); err != nil {
		logger.Panicf("Configuration file %v could not be read: %v", configurationFilePath, err)
	} else if err := yaml.Unmarshal(data, &configuration); err != nil ||
		configuration.CurrentDir == "" ||
		len(configuration.WatchedDirs) == 0 {
		logger.Panicf("Invalid configuration file %v: %v", configurationFilePath, err)
	}
	for _, configItem := range configuration.WatchedDirs {
		watchedDir := watchedDir{
			root:         configItem.Root,
			workItems:    make(chan workItem),
			workPackages: make(chan []workItem),
		}
		if configItem.AgglomerationTime == "" {
			watchedDir.agglomerationTime = 10 * time.Millisecond
		} else if ms, err := strconv.Atoi(configItem.AgglomerationTime); err != nil {
			logger.Panicf("Invalid configuration file %v: Agglomeration time %v is not an integer",
				configurationFilePath, configItem.AgglomerationTime)
		} else {
			watchedDir.agglomerationTime = time.Duration(ms) * time.Millisecond
		}
		for _, pattern := range configItem.Excludes {
			if excludeRegexp, err := regexp.Compile(pattern); err != nil {
				logger.Panicf("Invalid configuration file %v: Regexp %v is invalid",
					configurationFilePath, pattern)
			} else {
				watchedDir.excludeRegexps = append(watchedDir.excludeRegexps, excludeRegexp)
			}
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
				break
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
	if err := filepath.WalkDir(root,
		func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if err := watcher.Add(path); err != nil {
					logger.Printf("Could not add watch of directory %v: %v; ignoring", path, err)
				}
			}
			return nil
		}); err != nil {
		logger.Printf("Could not walk through directory %v: %v; ignoring", root, err)
	}
}

func eventsWatcher(ctx context.Context,
	watcher *fsnotify.Watcher, workItems chan<- workItem, excludeRegexps []*regexp.Regexp) {
	defer ctx.Value(wgKey).(*sync.WaitGroup).Done()
	for {
		select {
		case event := <-watcher.Events:
			if isExcluded(event.Name, excludeRegexps) {
				logger.Println("eventsWatcher: Ignored", event.Name)
				break
			}
			newWorkItem := workItem{path: event.Name}
			if event.Op&fsnotify.Create == fsnotify.Create ||
				event.Op&fsnotify.Write == fsnotify.Write ||
				event.Op&fsnotify.Chmod == fsnotify.Chmod {
				newWorkItem.eventType = modified
				info, err := os.Stat(event.Name)
				if err != nil {
					logger.Printf("eventsWatcher: Error when trying to stat %v: %v", event.Name, err)
					newWorkItem.nodeType = unknown
				} else if info.IsDir() {
					newWorkItem.nodeType = directory
					if event.Op&fsnotify.Create == fsnotify.Create {
						addWatches(watcher, event.Name)
					}
				} else {
					newWorkItem.nodeType = file
				}
			} else {
				newWorkItem.eventType = deleted
			}
			workItems <- newWorkItem
		case err := <-watcher.Errors:
			logger.Printf("eventsWatcher: Error %v (ignoring)", err)
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
	defer ctx.Value(wgKey).(*sync.WaitGroup).Done()
	scriptsDir := os.Args[1]
	for workPackage := range workPackages {
		var cmd *exec.Cmd
		if len(workPackage) > 1 {
			paths := make([]string, 0, len(workPackage))
			for _, workItem := range workPackage {
				paths = append(paths, workItem.path)
			}
			cmd = exec.Command(filepath.Join(scriptsDir, "bulk_sync"), longestPrefix(paths))
		} else {
			workItem := workPackage[0]
			if workItem.eventType == deleted {
				cmd = exec.Command(filepath.Join(scriptsDir, "delete"), workItem.path)
			} else if workItem.nodeType == file {
				cmd = exec.Command(filepath.Join(scriptsDir, "copy"), workItem.path)
			} else {
				cmd = exec.Command(filepath.Join(scriptsDir, "bulk_sync"), workItem.path)
			}
		}
		logger.Println("Start external command", cmd)
		if err := cmd.Start(); err != nil {
			logger.Println("Could not start external command:", err)
		} else if err := waitOrStop(ctx, cmd, syscall.SIGTERM, 100*time.Millisecond); err != nil {
			logger.Println("External command error:", err)
		}
	}
}

type key int

const wgKey key = 0

func main() {
	defer logger.Println("Exiting gracefully.")

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	defer wg.Wait()
	ctx = context.WithValue(ctx, wgKey, &wg)

	go func() {
		<-sigs
		cancel()
	}()

	watchedDirs, currentDir := readConfiguration()
	if err := os.Chdir(currentDir); err != nil {
		logger.Panicf("Could not set current working directory to %v", currentDir)
	}
	for _, watchedDir := range watchedDirs {
		wg.Add(3)
		go workMarshaller(ctx, watchedDir.workItems, watchedDir.workPackages, watchedDir.agglomerationTime)
		go worker(ctx, watchedDir.workPackages)
		var err error
		if watchedDir.watcher, err = fsnotify.NewWatcher(); err != nil {
			logger.Panic("Could not create new empty watcher")
		}
		go eventsWatcher(ctx, watchedDir.watcher, watchedDir.workItems, watchedDir.excludeRegexps)
		addWatches(watchedDir.watcher, watchedDir.root)
	}
}
