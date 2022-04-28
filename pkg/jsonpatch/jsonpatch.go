package jsonpatch

import (
	"bytes"

	"gomodules.xyz/jsonpatch/v2"
)

type Operations []jsonpatch.JsonPatchOperation

func (o Operations) MarshallJSON() ([]byte, error) {
	var b bytes.Buffer

	b.WriteString("[")
	for idx, op := range o {
		if idx > 0 {
			b.WriteString(",")
		}

		bs, err := op.MarshalJSON()
		if err != nil {
			return nil, err
		}
		b.Write(bs)
	}
	b.WriteString("]")

	return b.Bytes(), nil
}
