package main

import (
	"encoding/xml"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type multistatus struct {
	XMLName   xml.Name   `xml:"D:multistatus"`
	NS        string     `xml:"xmlns:D,attr"`
	Responses []response `xml:"D:response"`
}

type response struct {
	Href   string `xml:"D:href"`
	Prop   prop   `xml:"D:propstat>D:prop"`
	Status string `xml:"D:propstat>D:status"`
}

type prop struct {
	Length       *int64       `xml:"D:getcontentlength,omitempty"`
	ETag         string       `xml:"D:getetag,omitempty"`
	Modified     string       `xml:"D:getlastmodified"`
	ResourceType resourceType `xml:"D:resourcetype"`
}

type resourceType struct {
	Collection *struct{} `xml:"D:collection,omitempty"`
}

// propfind answers Depth 0 and 1 with the four live properties, whatever the request body asks for.
func propfind(w http.ResponseWriter, r *http.Request, p string, segs []string, slash bool) {
	depth := r.Header.Get("Depth")
	if depth != "0" && depth != "1" {
		fail(w, http.StatusNotImplemented, "only Depth: 0 and Depth: 1 are supported")
		return
	}
	fi, err := os.Stat(p)
	if err != nil {
		failFS(w, err)
		return
	}
	self, prefix := hrefs(segs, slash, fi.IsDir())
	first, err := describe(self, p, fi)
	if err != nil {
		failFS(w, err)
		return
	}
	ms := multistatus{NS: "DAV:", Responses: []response{first}}
	if depth == "1" && fi.IsDir() {
		entries, err := os.ReadDir(p)
		if err != nil {
			failFS(w, err)
			return
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				failFS(w, err)
				return
			}
			href := prefix + url.PathEscape(e.Name())
			if info.IsDir() {
				href += "/"
			}
			resp, err := describe(href, filepath.Join(p, e.Name()), info)
			if err != nil {
				failFS(w, err)
				return
			}
			ms.Responses = append(ms.Responses, resp)
		}
	}
	body, err := xml.Marshal(ms)
	if err != nil {
		failFS(w, err)
		return
	}
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

// hrefs are relative references resolved against the request URI (RFC 4918 §8.3). The gateway strips its
// own path prefix before forwarding, so a path-absolute href built from what this server sees would point
// outside the page's storage. self is the target's href; prefix goes in front of each child's name.
func hrefs(segs []string, slash, dir bool) (self, prefix string) {
	if len(segs) == 0 || slash {
		return "./", ""
	}
	last := url.PathEscape(segs[len(segs)-1])
	if dir {
		return last + "/", last + "/"
	}
	return last, ""
}

func describe(href, p string, fi fs.FileInfo) (response, error) {
	pr := prop{Modified: fi.ModTime().UTC().Format(http.TimeFormat)}
	if fi.IsDir() {
		pr.ResourceType.Collection = &struct{}{}
	} else {
		f, err := os.Open(p)
		if err != nil {
			return response{}, err
		}
		defer f.Close()
		tag, err := etag(f)
		if err != nil {
			return response{}, err
		}
		size := fi.Size()
		pr.Length, pr.ETag = &size, tag
	}
	return response{Href: href, Prop: pr, Status: "HTTP/1.1 200 OK"}, nil
}
