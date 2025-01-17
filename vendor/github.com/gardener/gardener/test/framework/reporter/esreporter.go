// Copyright 2019 Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package reporter

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/onsi/ginkgo/v2/config"
	"github.com/onsi/ginkgo/v2/reporters"
	"github.com/onsi/ginkgo/v2/types"
)

// TestSuiteMetadata describes the metadata of a whole test suite with all tests.
type TestSuiteMetadata struct {
	Name     string      `json:"name"`
	Phase    ESSpecPhase `json:"phase"`
	Tests    int         `json:"tests"`
	Failures int         `json:"failures"`
	Errors   int         `json:"errors"`
	Duration float64     `json:"duration"`
}

// TestCase is one instanace of a test execution
type TestCase struct {
	Metadata       *TestSuiteMetadata `json:"suite"`
	Name           string             `json:"name"`
	ShortName      string             `json:"shortName"`
	Labels         []string           `json:"labels,omitempty"`
	Phase          ESSpecPhase        `json:"phase"`
	FailureMessage *FailureMessage    `json:"failure,omitempty"`
	Duration       float64            `json:"duration"`
	SystemOut      string             `json:"system-out,omitempty"`
}

// FailureMessage describes the error and the log output if a error occurred.
type FailureMessage struct {
	Type    ESSpecPhase `json:"type"`
	Message string      `json:"message"`
}

// ESSpecPhase represents the phase of a test
type ESSpecPhase string

const (
	// SpecPhaseUnknown is a test that unknown or skipped
	SpecPhaseUnknown ESSpecPhase = "Unknown"
	// SpecPhasePending is a test that is still running
	SpecPhasePending ESSpecPhase = "Pending"
	// SpecPhaseSucceeded is a successfully completed test
	SpecPhaseSucceeded ESSpecPhase = "Succeeded"
	// SpecPhaseFailed is a failed test
	SpecPhaseFailed ESSpecPhase = "Failed"
	// SpecPhaseInterrupted is a test which execution time was longer than the specified timeout
	SpecPhaseInterrupted ESSpecPhase = "Interrupted"
)

// GardenerESReporter is a custom ginkgo exporter for gardener integration tests that write a summary of the tests in a
// elastic search json report.
type GardenerESReporter struct {
	suite         *TestSuiteMetadata
	testCases     []TestCase
	testSuiteName string

	filename string
	index    []byte
}

var matchLabel, _ = regexp.Compile(`\\[(.*?)\\]`)

// NewDeprecatedGardenerESReporter creates a new Gardener elasticsearch reporter.
// The json bulk will be stored in the passed filename in the given es index.
// It can be used with reporters.ReportViaDeprecatedReporter in ginkgo v2 to get the same reporting as in ginkgo v1.
// However, it must be invoked in ReportAfterSuite instead of passed to RunSpecsWithDefaultAndCustomReporters, hence
// it was renamed to force dependent repositories to adapt.
// Deprecated: this needs to be reworked to ginkgo's new reporting infrastructure, ReportViaDeprecatedReporter will be
// removed in a future version of ginkgo v2, see https://onsi.github.io/ginkgo/MIGRATING_TO_V2#removed-custom-reporters
func NewDeprecatedGardenerESReporter(filename, index string) reporters.DeprecatedReporter {
	reporter := &GardenerESReporter{
		filename:  filename,
		testCases: []TestCase{},
	}
	if index != "" {
		reporter.index = append([]byte(getESIndexString(index)), []byte("\n")...)
	}

	return reporter
}

// SuiteWillBegin is the first function that is invoked by ginkgo when a test suites starts.
// It is used to setup metadata information about the suite
func (reporter *GardenerESReporter) SuiteWillBegin(config config.GinkgoConfigType, summary *types.SuiteSummary) {
	reporter.suite = &TestSuiteMetadata{
		Name:  summary.SuiteDescription,
		Phase: SpecPhaseSucceeded,
	}
	reporter.testSuiteName = summary.SuiteDescription
}

// SpecDidComplete analysis the completed test and creates new es entry
func (reporter *GardenerESReporter) SpecDidComplete(specSummary *types.SpecSummary) {
	// do not report skipped tests
	if specSummary.State == types.SpecStateSkipped || specSummary.State == types.SpecStatePending {
		return
	}

	testCase := TestCase{
		Metadata:  reporter.suite,
		Name:      strings.Join(specSummary.ComponentTexts[1:], " "),
		ShortName: getShortName(specSummary.ComponentTexts[len(specSummary.ComponentTexts)-1]),
		Phase:     PhaseForState(specSummary.State),
		Labels:    parseLabels(strings.Join(specSummary.ComponentTexts[1:], " ")),
	}
	if specSummary.State == types.SpecStateFailed || specSummary.State == types.SpecStateInterrupted || specSummary.State == types.SpecStatePanicked {
		testCase.FailureMessage = &FailureMessage{
			Type:    PhaseForState(specSummary.State),
			Message: failureMessage(specSummary.Failure),
		}
		testCase.SystemOut = specSummary.CapturedOutput
		reporter.suite.Phase = SpecPhaseFailed
	}
	testCase.Duration = specSummary.RunTime.Seconds()
	reporter.testCases = append(reporter.testCases, testCase)
}

// SuiteDidEnd collects the metadata for the whole test suite and writes the results
// as elasticsearch json bulk to the specified location.
func (reporter *GardenerESReporter) SuiteDidEnd(summary *types.SuiteSummary) {
	reporter.suite.Tests = summary.NumberOfSpecsThatWillBeRun
	reporter.suite.Duration = math.Trunc(summary.RunTime.Seconds()*1000) / 1000
	reporter.suite.Failures = summary.NumberOfFailedSpecs
	reporter.suite.Errors = 0

	dir := filepath.Dir(reporter.filename)
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("Failed to create report file: %s\n", err.Error())
			return
		}
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			fmt.Printf("Failed to create report directory %s: %s\n", dir, err.Error())
			return
		}
	}

	file, err := os.Create(reporter.filename)
	if err != nil {
		fmt.Printf("Failed to create report file: %s\n\t%s", reporter.filename, err.Error())
	}
	defer func() {
		if err := file.Close(); err != nil {
			fmt.Printf("unable to close report file: %s", err.Error())
		}
	}()

	encoder := json.NewEncoder(file)
	for _, testCase := range reporter.testCases {
		if len(reporter.index) != 0 {
			if _, err := file.Write(reporter.index); err != nil {
				fmt.Printf("Failed to write index: %s", err.Error())
				return
			}
		}

		err = encoder.Encode(testCase)
		if err != nil {
			fmt.Printf("Failed to generate report\n\t%s", err.Error())
			return
		}
	}
}

// SpecWillRun is implemented as a noop to satisfy the reporter interface for ginkgo.
func (reporter *GardenerESReporter) SpecWillRun(specSummary *types.SpecSummary) {}

// BeforeSuiteDidRun is implemented as a noop to satisfy the reporter interface for ginkgo.
func (reporter *GardenerESReporter) BeforeSuiteDidRun(setupSummary *types.SetupSummary) {}

// AfterSuiteDidRun is implemented as a noop to satisfy the reporter interface for ginkgo.
func (reporter *GardenerESReporter) AfterSuiteDidRun(setupSummary *types.SetupSummary) {}

func failureMessage(failure types.SpecFailure) string {
	return fmt.Sprintf("%s\n%s\n%s", failure.ComponentCodeLocation.String(), failure.Message, failure.Location.String())
}

// parseLabels returns all labels of a test that have teh format [<label>]
func parseLabels(name string) []string {
	labels := matchLabel.FindAllString(name, -1)
	for i, label := range labels {
		labels[i] = strings.Trim(label, "[]")
	}
	return labels
}

// getShortName removes all labels from the test name
func getShortName(name string) string {
	short := matchLabel.ReplaceAllString(name, "")
	return strings.TrimSpace(short)
}

// getESIndexString returns a bulk index configuration string for a index.
func getESIndexString(index string) string {
	format := `{ "index": { "_index": "%s", "_type": "_doc" } }`
	return fmt.Sprintf(format, index)
}

// PhaseForState maps ginkgo spec states to internal elasticsearch used phases
func PhaseForState(state types.SpecState) ESSpecPhase {
	switch state {
	case types.SpecStatePending:
		return SpecPhasePending
	case types.SpecStatePassed:
		return SpecPhaseSucceeded
	case types.SpecStateFailed:
		return SpecPhaseFailed
	case types.SpecStateInterrupted:
		return SpecPhaseInterrupted
	case types.SpecStatePanicked:
		return SpecPhaseFailed
	default:
		return SpecPhaseUnknown
	}
}
