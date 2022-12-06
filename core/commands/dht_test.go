package commands

import (
	"testing"

	"github.com/ipfs/go-libipfs/namesys"

	ipns "github.com/ipfs/go-ipns"
	"github.com/libp2p/go-libp2p/core/test"
)

func TestKeyTranslation(t *testing.T) {
	pid := test.RandPeerIDFatal(t)
	pkname := namesys.PkKeyForID(pid)
	ipnsname := ipns.RecordKey(pid)

	pkk, err := escapeDhtKey("/pk/" + pid.Pretty())
	if err != nil {
		t.Fatal(err)
	}

	ipnsk, err := escapeDhtKey("/ipns/" + pid.Pretty())
	if err != nil {
		t.Fatal(err)
	}

	if pkk != pkname {
		t.Fatal("keys didn't match!")
	}

	if ipnsk != ipnsname {
		t.Fatal("keys didn't match!")
	}
}
