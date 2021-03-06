package ext

import (
	"regexp"
	"fmt"
)

const (
	E_TAG_NOT_SUPPORT = "E_TAG_NOT_SUPPORT"
	E_BAD_TAG	= "E_BAD_TAG"
)

var MAX_TAG_LEN = 100
var validTagFmt = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

type ExtVer uint8

//ext versions
// version for message has no ext
var NO_EXT_VER = ExtVer(uint8(0))
// version fo message has tag ext
var TAG_EXT_VER = ExtVer(uint8(2))


var noExt = NoExt{}
type NoExt []byte

func NewNoExt() NoExt {
	return noExt
}

func (n NoExt) ExtVersion() ExtVer {
	return NO_EXT_VER
}

func (n NoExt) GetBytes() []byte {
	return nil
}

type TagExt []byte

func NewTagExt(tagName []byte) (TagExt, error) {
	if !validateTag(tagName) {
		return nil, fmt.Errorf("invalid tag %v", tagName)
	}
	return TagExt(tagName), nil
}

func (tag TagExt) GetTagName() string {
	return string(tag)
}

//pass in []byte not nil
func validateTag(beValidated []byte) bool {
	if len(beValidated) > MAX_TAG_LEN {
		return false
	}
	return validTagFmt.Match(beValidated)
}

func (tag TagExt) ExtVersion() ExtVer {
	return TAG_EXT_VER
}

func (tag TagExt) GetBytes() []byte {
	return tag
}

func (tag TagExt) String() string {
	return string(tag)
}

type IExtContent interface {
	ExtVersion() ExtVer
	GetBytes()   []byte
}