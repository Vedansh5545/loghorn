package main

import "testing"

func TestChildService(t *testing.T) {
	if got := childService([]string{"PATH=/bin", "OTEL_SERVICE_NAME=shop-api"}); got != "shop-api" {
		t.Errorf("got %q", got)
	}
	if got := childService([]string{"OTEL_SERVICE_NAME=a", "OTEL_SERVICE_NAME=b"}); got != "b" {
		t.Errorf("last wins: got %q", got)
	}
	if got := childService([]string{"PATH=/bin"}); got != "" {
		t.Errorf("none: got %q", got)
	}
}
