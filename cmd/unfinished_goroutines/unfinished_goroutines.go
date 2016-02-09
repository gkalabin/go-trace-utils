package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"flag"
	"fmt"
	"internal/trace"
	"io"
	"log"
	"os"
	"sort"
	"strings"
)

var states = map[byte]string{
	0:  "EvNone",
	1:  "EvBatch",
	2:  "EvFrequency",
	3:  "EvStack",
	4:  "EvGomaxprocs",
	5:  "EvProcStart",
	6:  "EvProcStop",
	7:  "EvGCStart",
	8:  "EvGCDone",
	9:  "EvGCScanStart",
	10: "EvGCScanDone",
	11: "EvGCSweepStart",
	12: "EvGCSweepDone",
	13: "EvGoCreate",
	14: "EvGoStart",
	15: "EvGoEnd",
	16: "EvGoStop",
	17: "EvGoSched",
	18: "EvGoPreempt",
	19: "EvGoSleep",
	20: "EvGoBlock",
	21: "EvGoUnblock",
	22: "EvGoBlockSend",
	23: "EvGoBlockRecv",
	24: "EvGoBlockSelect",
	25: "EvGoBlockSync",
	26: "EvGoBlockCond",
	27: "EvGoBlockNet",
	28: "EvGoSysCall",
	29: "EvGoSysExit",
	30: "EvGoSysBlock",
	31: "EvGoWaiting",
	32: "EvGoInSyscall",
	33: "EvHeapAlloc",
	34: "EvNextGC",
	35: "EvTimerGoroutine",
	36: "EvFutileWakeup",
	37: "EvCount",
}

type Goroutine struct {
	ID          uint64
	ParentID    uint64
	parentStack Stack
	name        string
	create      int64
	destroy     int64
	lastStack   Stack
	lastState   string
}

func (g Goroutine) String() string {
	indentedStack := "\t" + strings.Replace(g.lastStack.String(), "\n", "\n\t", -1)
	return fmt.Sprintf("%s [%s]:\n%s", g.name, g.lastState, indentedStack)
}

type Stack []*trace.Frame

func (s Stack) String() string {
	var out bytes.Buffer
	for frameIdx, frame := range s {
		if frameIdx != 0 {
			out.WriteString("\n")
		}
		out.WriteString(fmt.Sprintf("%s [%s:%d]", frame.Fn, frame.File, frame.Line))
	}
	return out.String()
}

func main() {
	flag.Parse()
	binaryFileName := flag.Arg(0)
	traceFileName := flag.Arg(1)
	if traceFileName == "" || binaryFileName == "" {
		log.Fatalf("Usage: <binary file> <trace file>. I got '%s' '%s'", binaryFileName, traceFileName)
		os.Exit(1)
	}
	events, err := parseEvents(binaryFileName, traceFileName)
	if err != nil {
		panic(err)
	}
	gs := events2gs(events)
	unfinished := unfinishedGs(gs)
	grouped := uniq(unfinished)

	for _, group := range grouped {
		fmt.Printf("%d %s\n\n", len(group), group[0])
	}
}

func unfinishedGs(gs map[uint64]*Goroutine) map[uint64]*Goroutine {
	unfinishedGs := make(map[uint64]*Goroutine)
	for id, g := range gs {
		if g.destroy != 0 {
			continue
		}
		unfinishedGs[id] = g
	}
	return unfinishedGs
}

func uniq(gs map[uint64]*Goroutine) [][]*Goroutine {
	hash := func(g *Goroutine) string {
		h := md5.New()
		io.WriteString(h, g.name)
		io.WriteString(h, g.lastState)
		io.WriteString(h, g.lastStack.String())
		return fmt.Sprintf("%x", h.Sum(nil))
	}
	// first, group goroutines by its stack and state
	groupsMap := make(map[string][]*Goroutine)
	for _, g := range gs {
		id := hash(g)
		if _, hasGroup := groupsMap[id]; !hasGroup {
			groupsMap[id] = make([]*Goroutine, 0)
		}
		groupsMap[id] = append(groupsMap[id], g)
	}
	// then collect them to slice
	groupsList := make([][]*Goroutine, 0, len(groupsMap))
	for _, group := range groupsMap {
		groupsList = append(groupsList, group)
	}
	sort.Sort(byLen(groupsList))
	return groupsList
}

type byLen [][]*Goroutine

func (b byLen) Len() int           { return len(b) }
func (b byLen) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byLen) Less(i, j int) bool { return len(b[i]) > len(b[j]) }

func parseEvents(binaryFileName, traceFileName string) ([]*trace.Event, error) {
	tracef, err := os.Open(traceFileName)
	if err != nil {
		return nil, fmt.Errorf("failed to open trace file: %v", err)
	}
	defer tracef.Close()
	// Parse and symbolize.
	events, err := trace.Parse(bufio.NewReader(tracef))
	if err != nil {
		return nil, fmt.Errorf("failed to parse trace: %v", err)
	}
	err = trace.Symbolize(events, binaryFileName)
	if err != nil {
		return nil, fmt.Errorf("failed to symbolize trace: %v", err)
	}
	return events, nil
}

// extract list of goroutines from trace events
func events2gs(events []*trace.Event) map[uint64]*Goroutine {
	goroutines := make(map[uint64]*Goroutine)
	for _, ev := range events {
		switch ev.Type {
		case trace.EvGoCreate:
			g := &Goroutine{ID: ev.Args[0], create: ev.Ts}
			goroutines[g.ID] = g
		case trace.EvGoEnd:
			g := goroutines[ev.G]
			g.destroy = ev.Ts
		}
		g, found := goroutines[ev.G]
		if !found {
			continue
		}
		g.lastState = states[ev.Type]
		g.lastStack = ev.Stk
		if len(ev.Stk) > 0 && g.name == "" {
			g.name = ev.Stk[0].Fn
		}
	}

	// setup parent-child relations in second pass
	// parent goroutine goes before child, so we have to register all the goroutines first (first loop),
	// then we restore the relationship
	for _, ev := range events {
		if ev.Type != trace.EvGoCreate || ev.Link == nil {
			continue
		}
		if _, hasAncestor := goroutines[ev.Link.G]; !hasAncestor {
			continue
		}
		child := goroutines[ev.Link.G]
		child.ParentID = ev.G
		child.parentStack = ev.Stk
	}

	return goroutines
}
