package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"

	"github.com/chain/txvm/crypto/ed25519"
	"github.com/chain/txvm/protocol"
	"github.com/chain/txvm/protocol/bc"
	"github.com/chain/txvm/protocol/state"
	"github.com/chain/txvm/protocol/validation"
)

var modes = map[string]func([]string){
	"build":    build,
	"hash":     hash,
	"header":   header,
	"new":      newBlock,
	"sign":     sign,
	"tx":       tx,
	"validate": validate,
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	mode := os.Args[1]
	fn, ok := modes[mode]
	if !ok {
		usage()
	}

	fn(os.Args[2:])
}

func build(args []string) {
	fs := flag.NewFlagSet("build", flag.PanicOnError)

	var (
		timeStr = fs.String("time", "", "block timestamp")
		snapOut = fs.String("snapout", "", "output file for snapshot")
	)

	err := fs.Parse(args)
	must(err)

	var ts time.Time
	if *timeStr == "" {
		ts = time.Now()
	} else {
		ts, err = time.Parse(time.RFC3339, *timeStr)
		must(err)
	}
	timestampMS := bc.Millis(ts)

	bb := protocol.NewBlockBuilder()

	snapshotBits, err := ioutil.ReadAll(os.Stdin)
	must(err)
	snapshot := new(state.Snapshot)
	err = snapshot.FromBytes(snapshotBits)
	must(err)

	err = bb.Start(snapshot, timestampMS)
	must(err)

	for _, arg := range fs.Args() {
		txbits, err := ioutil.ReadFile(arg)
		must(err)
		rawTx := new(bc.RawTx)
		err = proto.Unmarshal(txbits, rawTx)
		must(err)
		tx, err := bc.NewTx(rawTx.Program, rawTx.Version, rawTx.Runlimit)
		must(err)
		err = bb.AddTx(bc.NewCommitmentsTx(tx))
		must(err)
	}

	ub, newSnapshot, err := bb.Build()
	must(err)

	if *snapOut != "" {
		newSnapshotBytes, err := newSnapshot.Bytes()
		err = ioutil.WriteFile(*snapOut, newSnapshotBytes, 0644)
		must(err)
	}

	b := &bc.Block{UnsignedBlock: ub}
	bbytes, err := b.Bytes()
	must(err)

	os.Stdout.Write(bbytes)
}

func newBlock(args []string) {
	fs := flag.NewFlagSet("new", flag.PanicOnError)

	var (
		quorum  = fs.Int("quorum", 0, "number of signatures required to authorize block")
		timeStr = fs.String("time", "", "block timestamp")
	)

	err := fs.Parse(args)
	must(err)

	pubkeysHex := fs.Args()
	var pubkeys []ed25519.PublicKey
	for _, pubkeyHex := range pubkeysHex {
		b, err := hex.DecodeString(pubkeyHex)
		must(err)
		if len(b) != ed25519.PublicKeySize {
			panic(fmt.Errorf("bad pubkey length %d, want 32", len(b)))
		}
		pubkeys = append(pubkeys, ed25519.PublicKey(b))
	}

	if *quorum < 0 || *quorum > len(pubkeys) {
		panic(fmt.Errorf("-quorum must be between 1 and %d", len(pubkeys)))
	}
	if *quorum == 0 {
		// There may be zero pubkeys, in which case *quorum will remain
		// zero. But if there are any pubkeys then quorum should be at
		// least 1.
		*quorum = len(pubkeys)
	}

	var ts time.Time
	if *timeStr == "" {
		ts = time.Now()
	} else {
		ts, err = time.Parse(time.RFC3339, *timeStr)
		must(err)
	}

	block, err := protocol.NewInitialBlock(pubkeys, *quorum, ts)
	must(err)

	blockBytes, err := block.Bytes()
	must(err)

	os.Stdout.Write(blockBytes)
}

func sign(args []string) {
	fs := flag.NewFlagSet("sign", flag.PanicOnError)
	prevHex := fs.String("prev", "", "previous block header (hex)")
	err := fs.Parse(args)
	must(err)

	prevBytes, err := hex.DecodeString(*prevHex)
	must(err)

	var prev bc.BlockHeader
	err = proto.Unmarshal(prevBytes, &prev)
	must(err)

	blockBytes, err := ioutil.ReadAll(os.Stdin)
	must(err)

	block := new(bc.Block)
	err = block.FromBytes(blockBytes)
	must(err)

	hash := block.Hash().Bytes()

	block, err = bc.SignBlock(block.UnsignedBlock, &prev, func(idx int) (interface{}, error) {
		if arg := fs.Arg(idx); arg != "" {
			prv, err := hex.DecodeString(arg)
			must(err)
			return ed25519.Sign(prv, hash), nil
		}
		return nil, nil
	})
	must(err)

	blockBytes, err = block.Bytes()
	must(err)

	os.Stdout.Write(blockBytes)
}

func validate(args []string) {
	fs := flag.NewFlagSet("validate", flag.PanicOnError)

	var (
		prevHex = fs.String("prev", "", "previous block header (hex)")
		noSig   = fs.Bool("nosig", false, "skip signature validation")
		noPrev  = fs.Bool("noprev", false, "skip validation against previous block")
	)

	err := fs.Parse(args)
	must(err)

	inp, err := ioutil.ReadAll(os.Stdin)
	must(err)

	var b bc.Block
	err = b.FromBytes(inp)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *prevHex != "" {
		prevBytes, err := hex.DecodeString(*prevHex)
		must(err)

		var prev bc.BlockHeader
		err = proto.Unmarshal(prevBytes, &prev)
		must(err)

		err = validation.Block(b.UnsignedBlock, &prev)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		// TODO(bobg): consider a way to validate the ConsensusRoot and
		// NoncesRoot too

		if !*noSig {
			err = validation.BlockSig(&b, prev.NextPredicate)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		return
	}

	if b.Height == 1 || *noPrev {
		err = validation.BlockOnly(b.UnsignedBlock)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	fmt.Fprintln(os.Stderr, "previous blockheader not supplied")
	os.Exit(1)
}

func hash(_ []string) {
	inp, err := ioutil.ReadAll(os.Stdin)
	must(err)

	var (
		bh *bc.BlockHeader
		rb bc.RawBlock
	)
	err = proto.Unmarshal(inp, &rb)
	if err != nil {
		bh = new(bc.BlockHeader)
		err = proto.Unmarshal(inp, bh)
		must(err)
	} else {
		bh = rb.Header
	}
	h := bh.Hash()
	os.Stdout.Write(h.Bytes())
}

func header(args []string) {
	fs := flag.NewFlagSet("header", flag.PanicOnError)
	pretty := fs.Bool("pretty", false, "show individual blockheader fields")

	err := fs.Parse(args)
	must(err)

	inp, err := ioutil.ReadAll(os.Stdin)
	must(err)

	var rb bc.RawBlock
	err = proto.Unmarshal(inp, &rb)
	must(err)

	if *pretty {
		var (
			bh      = rb.Header
			pubkeys []string
		)

		for _, p := range bh.NextPredicate.Pubkeys {
			pubkeys = append(pubkeys, hex.EncodeToString(p))
		}

		fmt.Printf("Version: %d\n", bh.Version)
		fmt.Printf("Height: %d\n", bh.Height)
		if bh.PreviousBlockId != nil {
			fmt.Printf("PreviousBlockId: %x\n", bh.PreviousBlockId.Bytes())
		} else {
			fmt.Printf("PreviousBlockId: nil\n")
		}
		fmt.Printf("TimestampMs: %d\n", bh.TimestampMs)
		fmt.Printf("Runlimit: %d\n", bh.Runlimit)
		fmt.Printf("RefsCount: %d\n", bh.RefsCount)
		fmt.Printf("TransactionsRoot: %x\n", bh.TransactionsRoot.Bytes())
		fmt.Printf("ContractsRoot: %x\n", bh.ContractsRoot.Bytes())
		fmt.Printf("NoncesRoot: %x\n", bh.NoncesRoot.Bytes())
		fmt.Printf("NextPredicate.Version: %d\n", bh.NextPredicate.Version)
		fmt.Printf("NextPredicate.Quorum: %d\n", bh.NextPredicate.Quorum)
		fmt.Printf("NextPredicate.Pubkeys: %s\n", strings.Join(pubkeys, " "))
		fmt.Printf("Transactions: %d\n", len(rb.Transactions))
		return
	}

	headerBytes, err := proto.Marshal(rb.Header)
	must(err)

	os.Stdout.Write(headerBytes)
}

func tx(args []string) {
	fs := flag.NewFlagSet("tx", flag.PanicOnError)

	var (
		raw    = fs.Bool("raw", false, "emit raw tx")
		pretty = fs.Bool("pretty", false, "show individual tx fields")
	)

	err := fs.Parse(args)
	must(err)

	args = fs.Args()
	if len(args) < 1 {
		usage()
	}

	idx, err := strconv.Atoi(args[0])
	must(err)

	if idx < 0 {
		panic("index out of range")
	}

	inp, err := ioutil.ReadAll(os.Stdin)
	must(err)

	var rb bc.RawBlock
	err = proto.Unmarshal(inp, &rb)
	must(err)

	if idx >= len(rb.Transactions) {
		panic("index out of range")
	}

	tx := rb.Transactions[idx]

	if *raw {
		txBytes, err := proto.Marshal(tx)
		must(err)
		os.Stdout.Write(txBytes)
		return
	}

	if *pretty {
		fmt.Printf("Version: %d\n", tx.Version)
		fmt.Printf("Runlimit: %d\n", tx.Runlimit)
		fmt.Printf("Program: %x\n", tx.Program)
		return
	}

	os.Stdout.Write(tx.Program)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  block validate [-prev PREVHEX] [-nosig] [-noprev] <BLOCK")
	fmt.Fprintln(os.Stderr, "  block hash <BLOCK_OR_HEADER")
	fmt.Fprintln(os.Stderr, "  block header [-pretty] <BLOCK")
	fmt.Fprintln(os.Stderr, "  block tx [-raw] [-pretty] INDEX <BLOCK")
	fmt.Fprintln(os.Stderr, "  block new [-quorum QUORUM] [-time TIME] PUBKEYHEX PUBKEYHEX ... >BLOCK")
	fmt.Fprintln(os.Stderr, "  block build [-time TIME] [-snapout FILE] TXFILE TXFILE ... <SNAPSHOT >BLOCK")
	fmt.Fprintln(os.Stderr, "  block sign -prev PREVHEX PRVHEX PRVHEX ... <BLOCK >BLOCK")
	os.Exit(1)
}
