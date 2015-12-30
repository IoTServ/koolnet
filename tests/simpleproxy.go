package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
)

// A very simple http proxy

func main() {
	simpleProxyHandler := http.HandlerFunc(simpleProxyHandlerFunc)
	http.ListenAndServe(":8888", simpleProxyHandler)
}

func simpleProxyHandlerFunc(w http.ResponseWriter, r *http.Request) {
	//httpc := &http.Client{}

	// request uri can't be set in client requests
	r.RequestURI = ""

	if r.Method == "CONNECT" {
		fmt.Println(r.URL, " CONNECT / SSL not implemented")
		return
	}

	body_in, err := ioutil.ReadAll(r.Body)
	url := fmt.Sprintf("%v", r.URL)
	fmt.Printf("Url: %s\n", url)
	fmt.Printf("BodyIn: %v\n", string(body_in))
	bf_in := bytes.NewBuffer(body_in)

	rr, err := http.NewRequest(r.Method, url, bf_in)
	copyHeader(r.Header, &rr.Header)

	//resp, err := httpc.Do(r)
	var transport http.Transport
	resp, err := transport.RoundTrip(rr)

	if err != nil {
		fmt.Println(r.URL, " ", err.Error())
		return
	}

	fmt.Printf("Resp-Headers: %v\n", resp.Header)
	body, err := ioutil.ReadAll(resp.Body)
	fmt.Printf("Body: %v\n", string(body))
	bf := bytes.NewBuffer(body)

	copyHeaders(w.Header(), resp.Header)

	// copy content
	io.Copy(w, bf)
}

func copyHeaders(dest http.Header, source http.Header) {
	for header := range source {
		dest.Add(header, source.Get(header))
	}
}

func copyHeader(source http.Header, dest *http.Header) {
	for n, v := range source {
		for _, vv := range v {
			dest.Add(n, vv)
		}
	}
}
