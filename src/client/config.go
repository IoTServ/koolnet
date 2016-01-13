package client

import (
	"encoding/json"
	"io/ioutil"
)

type ConfigJson struct {
	Listen   []string `json:"listen"`
	Server   string   `json:"server"`
	Port     int      `json:"port"`
	User     string   `json:"user"`
	Password string   `json:"password"`
}

func readConfig(path string) (*ConfigJson, error) {
	file, e := ioutil.ReadFile(path)
	if e != nil {
		return nil, e
	}

	var conf ConfigJson
	json.Unmarshal(file, &conf)
	return &conf, nil
}
