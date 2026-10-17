package server

import "testing"

func TestImagineJobSnapshotCopiesImages(t *testing.T) {
	list := newImagineJobList()
	job := &imagineJob{ID: "job", Images: []ImagineGalleryItem{{File: "first.png"}}}
	list.add(job)

	snapshot := list.snapshot()
	snapshot[0].Images[0].File = "changed.png"
	if got := list.snapshot()[0].Images[0].File; got != "first.png" {
		t.Fatalf("snapshot mutation changed stored image: got %q", got)
	}
}
