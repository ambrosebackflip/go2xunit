package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"regexp"
	"strings"
	"text/template"
)

const (
	version = "1.1.1"

	// gotest regular expressions

	// === RUN TestAdd
	gt_startRE = "^=== RUN:? ([a-zA-Z_][^[:space:]]*)"

	// --- PASS: TestSub (0.00 seconds)
	// --- FAIL: TestSubFail (0.00 seconds)
	// --- SKIP: TestSubSkip (0.00 seconds)
	gt_endRE = "^--- (PASS|FAIL|SKIP): ([a-zA-Z_][^[:space:]]*) \\((\\d+(.\\d+)?)"

	// FAIL	_/home/miki/Projects/goroot/src/xunit	0.004s
	// ok  	_/home/miki/Projects/goroot/src/anotherTest	0.000s
	gt_suiteRE = "^(ok|FAIL)[ \t]+([^ \t]+)[ \t]+(\\d+.\\d+)"

	// ?       alipay  [no test files]
	gt_noFiles = "^\\?.*\\[no test files\\]$"
	// FAIL    node/config [build failed]
	gt_buildFailed = `^FAIL.*\[(build|setup) failed\]$`

	// gocheck regular expressions

	// START: mmath_test.go:16: MySuite.TestAdd
	gc_startRE = "START: [^:]+:[^:]+: ([A-Za-z_][[:word:]]*).([A-Za-z_][[:word:]]*)"
	// PASS: mmath_test.go:16: MySuite.TestAdd	0.000s
	// FAIL: mmath_test.go:35: MySuite.TestDiv
	gc_endRE = "(PASS|FAIL|SKIP|PANIC|MISS): [^:]+:[^:]+: ([A-Za-z_][[:word:]]*).([A-Za-z_][[:word:]]*)([[:space:]]+([0-9]+.[0-9]+))?"

	gc_timeoutRE = "(\\*)+ Test killed with quit: ran too long \\(([A-Za-z0-9][[:word:]]*)\\)"
)

var (
	failOnRace = false
)

const (
	Unknown = iota
	Failed  = iota
	Skipped = iota
	Passed  = iota
)

type TestResult int

type Test struct {
	Name, Time, Message, Suite string
	Result                     TestResult
}

func (t *Test) Failed() bool {
	return t.Result == Failed
}

func (t *Test) Skipped() bool {
	return t.Result == Skipped
}

func (t *Test) Passed() bool {
	return t.Result == Passed
}

type Suite struct {
	Name   string
	Time   string
	Status string
	Tests  []*Test
}

type SuiteStack struct {
	nodes []*Suite
	count int
}

// Push adds a node to the stack.
func (s *SuiteStack) Push(n *Suite) {
	s.nodes = append(s.nodes[:s.count], n)
	s.count++
}

// Pop removes and returns a node from the stack in last to first order.
func (s *SuiteStack) Pop() *Suite {
	if s.count == 0 {
		return nil
	}
	s.count--
	return s.nodes[s.count]
}

func (suite *Suite) NumFailed() int {
	count := 0
	for _, test := range suite.Tests {
		if test.Result == Failed {
			count++
		}
	}

	return count
}

func (suite *Suite) NumSkipped() int {
	count := 0
	for _, test := range suite.Tests {
		if test.Result == Skipped {
			count++
		}
	}

	return count
}

func (suite *Suite) Count() int {
	return len(suite.Tests)
}

func hasDatarace(lines []string) bool {
	has_datarace := regexp.MustCompile("^WARNING: DATA RACE$").MatchString
	for _, line := range lines {
		if has_datarace(line) {
			return true
		}
	}
	return false
}

func gt_Parse(rd io.Reader) ([]*Suite, error) {
	find_start := regexp.MustCompile(gt_startRE).FindStringSubmatch
	find_end := regexp.MustCompile(gt_endRE).FindStringSubmatch
	find_suite := regexp.MustCompile(gt_suiteRE).FindStringSubmatch
	is_nofiles := regexp.MustCompile(gt_noFiles).MatchString
	is_buildFailed := regexp.MustCompile(gt_buildFailed).MatchString
	is_exit := regexp.MustCompile("^exit status -?\\d+").MatchString

	suites := []*Suite{}
	var curTest *Test
	var curSuite *Suite
	var out []string
	suiteStack := SuiteStack{}
	// Handles a test that ended with a panic.
	handlePanic := func() {
		curTest.Result = Failed
		curTest.Time = "N/A"
		curSuite.Tests = append(curSuite.Tests, curTest)
		curTest = nil
	}

	// Appends output to the last test.
	appendError := func() error {
		if len(out) > 0 && curSuite != nil && len(curSuite.Tests) > 0 {
			message := strings.Join(out, "\n")
			if curSuite.Tests[len(curSuite.Tests)-1].Message == "" {
				curSuite.Tests[len(curSuite.Tests)-1].Message = message
			} else {
				curSuite.Tests[len(curSuite.Tests)-1].Message += "\n" + message
			}
		}
		out = []string{}
		return nil
	}

	scanner := bufio.NewScanner(rd)
	for lnum := 1; scanner.Scan(); lnum++ {
		line := scanner.Text()

		// TODO: Only outside a suite/test, report as empty suite?
		if is_nofiles(line) {
			continue
		}

		if is_buildFailed(line) {
			return nil, fmt.Errorf("%d: package build failed: %s", lnum, line)
		}

		if curSuite == nil {
			curSuite = &Suite{}
		}

		tokens := find_start(line)
		if tokens != nil {
			if curTest != nil {
				// This occurs when the last test ended with a panic.
				if suiteStack.count == 0 {
					suiteStack.Push(curSuite)
					curSuite = &Suite{Name: curTest.Name}
				} else {
					handlePanic()
				}
			}
			if e := appendError(); e != nil {
				return nil, e
			}
			curTest = &Test{
				Name: tokens[1],
			}
			continue
		}

		tokens = find_end(line)
		if tokens != nil {
			if curTest == nil {
				if suiteStack.count > 0 {
					prevSuite := suiteStack.Pop()
					suites = append(suites, curSuite)
					curSuite = prevSuite
					continue
				} else {
					return nil, fmt.Errorf("%d: orphan end test", lnum)
				}
			}
			if tokens[2] != curTest.Name {
				err := fmt.Errorf("%d: name mismatch (try disabling parallel mode)", lnum)
				return nil, err
			}
			if tokens[1] == "FAIL" || failOnRace && hasDatarace(out) {
				curTest.Result = Failed
			} else if tokens[1] == "SKIP" {
				curTest.Result = Skipped
			} else {
				curTest.Result = Passed
			}
			curTest.Time = tokens[3]
			curTest.Message = strings.Join(out, "\n")
			curSuite.Tests = append(curSuite.Tests, curTest)
			curTest = nil
			out = []string{}
			continue
		}

		tokens = find_suite(line)
		if tokens != nil {
			if curTest != nil {
				// This occurs when the last test ended with a panic.
				handlePanic()
			}
			if e := appendError(); e != nil {
				return nil, e
			}
			curSuite.Name = tokens[2]
			curSuite.Time = tokens[3]
			suites = append(suites, curSuite)
			curSuite = nil
			continue
		}

		if is_exit(line) || (line == "FAIL") || (line == "PASS") {
			continue
		}

		out = append(out, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return suites, nil
}

func map2arr(m map[string]*Suite) []*Suite {
	arr := make([]*Suite, 0, len(m))
	for _, suite := range m {
		/* FIXME:
		suite.Status =
		suite.Time =
		*/
		arr = append(arr, suite)
	}

	return arr
}

func shouldSkipTestWithName(testName string) bool {
	if testName == "SetUpTest" ||
		testName == "SetUpSuite" ||
		testName == "TearDownTest" ||
		testName == "TearDownSuite" {
		return true
	}
	return false
}

type testCursor struct {
	testStack []string
	suite     string
}

func (t *testCursor) push(suite, test string) {
	t.suite = suite
	t.testStack = append(t.testStack, test)
}

func (t *testCursor) pop(suite, test string) {
	t.testStack = t.testStack[:len(t.testStack)-1]
}

func (t *testCursor) peek() string {
	return t.testStack[len(t.testStack)-1]
}

// gc_Parse parses output of "go test -gocheck.vv", returns a list of tests
// See data/gocheck.out for an example
func gc_Parse(rd io.Reader) ([]*Suite, error) {
	find_start := regexp.MustCompile(gc_startRE).FindStringSubmatch
	find_end := regexp.MustCompile(gc_endRE).FindStringSubmatch
	find_timeout := regexp.MustCompile(gc_timeoutRE).FindStringSubmatch

	scanner := bufio.NewScanner(rd)
	var suites = make(map[string]*Suite)
	var out []string

	cursor := &testCursor{}

	for lnum := 1; scanner.Scan(); lnum++ {
		line := scanner.Text()

		// If this is the start of a test
		tokens := find_start(line)
		if len(tokens) > 0 {
			suiteName := tokens[1]
			testName := tokens[2]

			cursor.push(suiteName, testName)

			// Clear the output
			out = []string{}

			if !shouldSkipTestWithName(testName) {
				// Find the suite that the test belongs to or create a new one
				suite, ok := suites[suiteName]
				if !ok {
					suite = &Suite{Name: suiteName}
					suites[suiteName] = suite
				}

				// Find the test or create a new one
				var test *Test = nil
				for i, v := range suite.Tests {
					if v.Name == testName {
						test = suite.Tests[i]
						break
					}
				}
				if test == nil {
					test = &Test{Name: testName, Result: Unknown}
					suite.Tests = append(suite.Tests, test)
				}
			}
			continue
		}

		// If this is the conclusion of a test
		tokens = find_end(line)
		if len(tokens) > 0 {
			suiteName := tokens[2]
			testName := tokens[3]

			cursor.pop(suiteName, testName)

			if !shouldSkipTestWithName(testName) {
				// Find the suite
				suite, ok := suites[suiteName]
				if !ok {
					return nil, fmt.Errorf("%d: orphan end (suite)", lnum)
				}

				// Find the test
				var test *Test = nil
				for i, v := range suite.Tests {
					if v.Name == testName {
						test = suite.Tests[i]
						break
					}
				}
				if test == nil {
					return nil, fmt.Errorf("%d: orphan end (test)", lnum)
				}

				// Update the test results
				test.Message = strings.Join(out, "\n")
				test.Time = tokens[4]
				if tokens[1] == "FAIL" || tokens[1] == "PANIC" || tokens[1] == "MISS" {
					test.Result = Failed
				} else if tokens[1] == "SKIP" {
					test.Result = Skipped
				} else {
					test.Result = Passed
				}
			}
			// Clear the output
			out = []string{}
			continue
		}

		if strings.HasPrefix(line, "*** Test killed with quit: ran too long") {
			tokens = find_timeout(line)
			if len(tokens) == 0 {
				panic("failed to find timeout time")
			} else {
				for i, t := range tokens {
					fmt.Printf("%d: %s\n", i, t)
				}
			}

			suiteName := cursor.suite
			testName := cursor.peek()

			cursor.pop(suiteName, testName)

			if !shouldSkipTestWithName(testName) {
				// Find the suite
				suite, ok := suites[suiteName]
				if !ok {
					return nil, fmt.Errorf("%d: orphan end (suite)", lnum)
				}

				// Find the test
				var test *Test = nil
				for i, v := range suite.Tests {
					if v.Name == testName {
						test = suite.Tests[i]
						break
					}
				}
				if test == nil {
					return nil, fmt.Errorf("%d: orphan end (test)", lnum)
				}

				// Update the test results
				test.Message = strings.Join(out, "\n")
				test.Time = tokens[2]
				test.Result = Failed
			}
			// Clear the output
			out = []string{}
			continue
		}

		out = append(out, line)
	}

	for _, suite := range suites {
		for _, test := range suite.Tests {
			if test.Result == Unknown {
				return nil, fmt.Errorf("unknown test result for test %s in suite %s", test.Name, suite.Name)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return map2arr(suites), nil
}

func hasFailures(suites []*Suite) bool {
	for _, suite := range suites {
		if suite.NumFailed() > 0 {
			return true
		}
	}
	return false
}

var xmlTemplate string = `<?xml version="1.0" encoding="utf-8"?>
<testsuite name="{{.Name}}" tests="{{.Count}}" errors="0" failures="{{.NumFailed}}" skip="{{.NumSkipped}}">
{{range  $test := .Tests}}    
<testcase classname="{{$test.Suite}}" name="{{$test.Name}}" time="{{$test.Time}}">
{{if $test.Skipped }}      
<skipped/> 
{{end}}
{{if $test.Failed }}      
<failure type="go.error" message="error">
<![CDATA[{{$test.Message}}]]>
</failure>
{{end}}    
</testcase>
{{end}}  
</testsuite>
`

// writeXML exits xunit XML of tests to out
func writeXML(suites []*Suite, outputDir string) error {
	_, derr := os.Stat(outputDir)
	if derr != nil {
		if derr = os.MkdirAll(outputDir, 0777); derr != nil {
			return derr
		}
	}

	for _, suite := range suites {
		resultFile := path.Join(outputDir, strings.Replace(suite.Name, "/", "_", -1)+".xml")
		out, cerr := os.Create(resultFile)
		if cerr != nil {
			fmt.Printf("Unable to create file: %s (%s)\n", resultFile, cerr)
			return cerr
		}

		t := template.New("test template")
		t, perr := t.Parse(xmlTemplate)
		if perr != nil {
			fmt.Printf("Error in parse %v\n", perr)
			return perr
		}
		eerr := t.Execute(out, suite)
		if eerr != nil {
			fmt.Printf("Error in execute %v\n", eerr)
			return eerr
		}
	}
	return nil
}

// getInput return input io.Reader from file name, if file name is - it will
// return os.Stdin
func getInput(filename string) (io.Reader, error) {
	if filename == "-" || filename == "" {
		return os.Stdin, nil
	}

	return os.Open(filename)
}

// getIO returns input and output streams from file names
func getIO(inputFile string) (io.Reader, error) {
	input, err := getInput(inputFile)
	if err != nil {
		return nil, fmt.Errorf("can't open %s for reading: %s", inputFile, err)
	}

	return input, nil
}

func main() {
	inputFile := flag.String("input", "", "input file (default to stdin)")
	outputDir := flag.String("output", "", "output directory")
	fail := flag.Bool("fail", false, "fail (non zero exit) if any test failed")
	showVersion := flag.Bool("version", false, "print version and exit")
	is_gocheck := flag.Bool("gocheck", false, "parse gocheck output")
	flag.BoolVar(&failOnRace, "fail-on-race", false, "mark test as failing if it exposes a data race")
	flag.Parse()

	if *showVersion {
		fmt.Printf("go2xunit %s\n", version)
		os.Exit(0)
	}

	if len(*outputDir) == 0 {
		log.Fatalf("error: output directory is required (-output)")
	}

	// No time ... prefix for error messages
	log.SetFlags(0)

	if flag.NArg() > 0 {
		log.Fatalf("error: %s does not take parameters (did you mean -input?)", os.Args[0])
	}

	input, err := getIO(*inputFile)
	if err != nil {
		log.Fatalf("error: %s", err)
	}

	var parse func(rd io.Reader) ([]*Suite, error)

	if *is_gocheck {
		parse = gc_Parse
	} else {
		parse = gt_Parse
	}

	suites, err := parse(input)
	if err != nil {
		log.Fatalf("error: %s", err)
	}
	if len(suites) == 0 {
		log.Fatalf("error: no tests found")
		os.Exit(1)
	}

	for _, suite := range suites {
		for i := 0; i < len(suite.Tests); i++ {
			suite.Tests[i].Suite = suite.Name
		}
	}

	writeXML(suites, *outputDir)
	if *fail && hasFailures(suites) {
		os.Exit(1)
	}
}
