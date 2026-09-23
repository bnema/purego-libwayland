// Package protocol holds generated server bindings for vendored Wayland protocols.
package protocol

//go:generate go run ../cmd/wlgen -package wayland -out wayland/wayland.go ../protocols/wayland.xml
//go:generate go run ../cmd/wlgen -package xdgshell -out xdgshell/xdgshell.go -import wayland=github.com/bnema/purego-libwayland/protocol/wayland -import-xml wayland=../protocols/wayland.xml ../protocols/xdg-shell.xml
