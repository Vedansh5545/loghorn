package main

import (
	"strings"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/ghjob"
)

func TestDescribeJobs(t *testing.T) {
	jobs := []ghjob.Job{
		{ID: 11, Name: "lint", Conclusion: "success"},
		{ID: 22, Name: "test-linux", Conclusion: "failure"},
		{ID: 33, Name: "build", Status: "in_progress"},
	}
	var b strings.Builder
	describeJobs(&b, "900", jobs, 1)
	want := "loghorn: run 900 has 3 jobs; showing the first failed one (pick another with -gh-job <id>):\n" +
		"    11  lint        success\n" +
		"  * 22  test-linux  failure\n" +
		"    33  build       in_progress\n"
	if b.String() != want {
		t.Errorf("describeJobs (failed) =\n%s\nwant\n%s", b.String(), want)
	}

	ok := []ghjob.Job{
		{ID: 11, Name: "lint", Conclusion: "success"},
		{ID: 22, Name: "test", Conclusion: "success"},
	}
	b.Reset()
	describeJobs(&b, "901", ok, 0)
	want = "loghorn: run 901 has 2 jobs; showing the first one (pick another with -gh-job <id>):\n" +
		"  * 11  lint  success\n" +
		"    22  test  success\n"
	if b.String() != want {
		t.Errorf("describeJobs (none failed) =\n%s\nwant\n%s", b.String(), want)
	}
}
