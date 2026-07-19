package main

import (
	"bytes"
	"testing"

	"golang.org/x/net/html"
)

func TestNativeChatScrimSharesShellStackingContext(t *testing.T) {
	data, err := assets.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	document, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	scrim := htmlElementByID(document, "nativeChatScrim")
	if scrim == nil {
		t.Fatal("nativeChatScrim not found")
	}
	if scrim.Parent == nil || !htmlElementHasClass(scrim.Parent, "nativeChatShell") {
		t.Fatal("nativeChatScrim must be a direct child of nativeChatShell so it stays below the mobile side panels")
	}
}

func htmlElementByID(node *html.Node, id string) *html.Node {
	if node.Type == html.ElementNode {
		for _, attribute := range node.Attr {
			if attribute.Key == "id" && attribute.Val == id {
				return node
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := htmlElementByID(child, id); found != nil {
			return found
		}
	}
	return nil
}

func htmlElementHasClass(node *html.Node, className string) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}
	for _, attribute := range node.Attr {
		if attribute.Key != "class" {
			continue
		}
		for _, current := range bytes.Fields([]byte(attribute.Val)) {
			if string(current) == className {
				return true
			}
		}
	}
	return false
}
