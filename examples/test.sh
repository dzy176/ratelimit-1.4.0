#!/bin/sh


curl -X POST -d '{"domain":"rl","descriptors":[{"entries":[{"key":"user","value":"admin"}]},{"entries":[{"key":"user","value":"foo"}, {"key":"age","value":"19"}]},{"entries":[{"key":"user","value":"admin"},{"key":"age","value":"10"}]}]}' http://127.0.0.1:8080/json

