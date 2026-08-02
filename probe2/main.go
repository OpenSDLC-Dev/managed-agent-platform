package main

import (
	"encoding/json"
	"fmt"
)

func try(s string) {
	var n json.Number
	err := json.Unmarshal([]byte(s), &n)
	if err != nil {
		fmt.Printf("%-10s unmarshal err: %v\n", s, err)
		return
	}
	v, err := n.Int64()
	fmt.Printf("%-10s number=%q int64=%d err=%v\n", s, string(n), v, err)
}

func main() {
	for _, s := range []string{`5`, `"5"`, `2.0`, `2.5`, `1e2`, `"0x5"`, `-3`, `20`, `"20"`, `true`, `"1e1"`} {
		try(s)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"type":"text","type":"file","file_id":"f","content":"c"}`), &m)
	out, _ := json.Marshal(m)
	fmt.Println("dup-key remarshal:", string(out))
	// lone surrogate round trip
	var s string
	raw := json.RawMessage(`"\ud800abc"`)
	_ = json.Unmarshal(raw, &s)
	fmt.Printf("decoded=%q len=%d\n", s, len([]rune(s)))
	m2 := map[string]json.RawMessage{"type": json.RawMessage(`"text"`), "content": raw}
	o2, _ := json.Marshal(m2)
	fmt.Println("rubric remarshal:", string(o2))
}
