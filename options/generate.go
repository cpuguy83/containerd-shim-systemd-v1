package options

//go:generate sh -c "cd .. && protoc --plugin=protoc-gen-go=\"$DOLLAR(go tool -n protoc-gen-go)\" --go_out=. --go_opt=paths=source_relative options/options.proto"
