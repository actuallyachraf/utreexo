package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"golang.org/x/crypto/blake2b"
)

type Hash [32]byte

func HashFromString(s string) Hash {
	return blake2b.Sum256([]byte(s))
}

const txoFilename = "mainnet.txos"

/* first pass, just add the deletions to the DB.
then go another pass, and just check the adds, making new lines
of their ttls */
func firstPass() error {
	txofile, err := os.OpenFile(txoFilename, os.O_RDONLY, 0600)
	if err != nil {
		return err
	}
	defer txofile.Close()

	scanner := bufio.NewScanner(txofile)

	var height uint32
	height = 1
	// height starts at 1 because there are no transactions in block 0

	//	var dels sortableDelunit
	var wg sync.WaitGroup

	// make the channel ... have a buffer? does it matter?
	batchan := make(chan *leveldb.Batch)

	// open database
	o := new(opt.Options)
	o.CompactionTableSizeMultiplier = 8
	lvdb, err := leveldb.OpenFile("./lvdb", o)
	if err != nil {
		panic(err)
	}
	defer lvdb.Close()

	batch := new(leveldb.Batch)
	//start db writer worker... actually start a bunch of em
	// try 1 worker...?
	for j := 0; j < 16; j++ {
		go dbWorker(batchan, lvdb, &wg)
	}

	for scanner.Scan() {
		switch scanner.Text()[0] {
		case '-':
			h := HashFromString(scanner.Text()[1:])
			batch.Put(h[:], U32tB(height))
			//			dels = append(dels, delUnit{h, height})
		case '+':
			// do nothing
		case 'h':
			if height%1000 == 0 {
				fmt.Printf("have %d deletions at height %d\n", batch.Len(), height)
			}
			// save 1M at a time, 32MB
			if batch.Len() > 1000000 {
				fmt.Printf("--- sending off %d dels at height %d\n", batch.Len(), height)
				wg.Add(1)
				batchan <- batch
				batch = new(leveldb.Batch)
				//				dels = dels[:0]
			}
			height++
		default:
			panic("unknown string")
		}
	}
	// done, but there's stuff left
	wg.Add(1)
	fmt.Printf("--- sending off %d dels at height %d\n", batch.Len(), height)
	batchan <- batch
	wg.Wait()

	return nil
}

// dbWorker writes everything to the db.  It's it's own goroutine so it
// can work at the same time that the reads are happening
func dbWorker(
	bChan chan *leveldb.Batch, lvdb *leveldb.DB, wg *sync.WaitGroup) {

	for {
		b := <-bChan

		fmt.Printf("--- writing batch %d dels\n", b.Len())

		err := lvdb.Write(b, nil)
		if err != nil {
			fmt.Println(err.Error())
		}
		fmt.Printf("wrote %d deletions to leveldb\n", b.Len())
		wg.Done()
	}
}

// for parallel txofile building we need to have a buffer
type txotx struct {
	// the inputs as a string.  Note that this string has \ns in it, and it's
	// the whole block of inputs.
	// also includes the whole + line with the output!
	// includes the +, the txid the ; z and ints, but no x, ints or commas
	// (basically whatever lit produced in mainnet.txos)
	txText string

	outputtxid string
	// here's all the death heights of the output txos
	deathHeights []uint32
}

type deathInfo struct {
	deathHeight, blockPos, txPos uint32
}

// for each block, make a slice of txotxs in order.  The slice will stay in order.
// also make the deathheights slices for all the txotxs the right size.
// then hand the []txotx slice over to the worker function which can make the
// lookups in parallel and populate the deathheights.  From there you can go
// back to serial to write back to the txofile.

// ttlLookup takes the slice of txotxs and fills in the deathheights
func lookupBlock(block []*txotx, db *leveldb.DB) {

	// I don't think buffering this will do anything..?
	infoChan := make(chan deathInfo)

	var remaining uint32

	// go through every tx
	for blockPos, tx := range block {
		// go through every output
		for txPos, _ := range tx.deathHeights {
			// increment counter, and send off to a worker
			remaining++
			go lookerUpperWorker(
				tx.outputtxid, uint32(blockPos), uint32(txPos), infoChan, db)
		}
	}

	var rcv deathInfo
	for remaining > 0 {
		//		fmt.Printf("%d left\t", remaining)
		rcv = <-infoChan
		block[rcv.blockPos].deathHeights[rcv.txPos] = rcv.deathHeight
		remaining--
	}

	return
}

// lookerUpperWorker does the hashing and db read, then returns it's result
// via a channel
func lookerUpperWorker(
	txid string, blockPos, txPos uint32,
	infoChan chan deathInfo, db *leveldb.DB) {

	// start deathInfo struck to send back
	var di deathInfo
	di.blockPos, di.txPos = blockPos, txPos

	// build string and hash it (nice that this in parallel too)
	utxostring := fmt.Sprintf("%s;%d", txid, txPos)
	opHash := HashFromString(utxostring)

	// make DB lookup
	ttlbytes, err := db.Get(opHash[:], nil)
	if err == leveldb.ErrNotFound {
		//		fmt.Printf("can't find %s;%d in file", txid, txPos)
		ttlbytes = make([]byte, 4) // not found is 0
	} else if err != nil {
		// some other error
		panic(err)
	}
	if len(ttlbytes) != 4 {
		fmt.Printf("val len %d, op %s;%d\n", len(ttlbytes), txid, txPos)
		panic("ded")
	}

	di.deathHeight = BtU32(ttlbytes)
	// send back to the channel and this output is done
	infoChan <- di

	return
}

// read from the DB and tack on TTL values
func secondPass() error {

	// open database
	o := new(opt.Options)
	o.CompactionTableSizeMultiplier = 8
	o.ReadOnly = true
	lvdb, err := leveldb.OpenFile("./lvdb", o)
	if err != nil {
		panic(err)
	}
	defer lvdb.Close()

	txofile, err := os.OpenFile(txoFilename, os.O_RDONLY, 0600)
	if err != nil {
		return err
	}
	defer txofile.Close()
	ttlfile, err := os.OpenFile("ttl."+txoFilename, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer ttlfile.Close()

	scanner := bufio.NewScanner(txofile)

	var height uint32

	height = 1
	// height starts at 1 because there are no transactions in block 0

	blocktxs := []*txotx{new(txotx)}

	for scanner.Scan() {
		switch scanner.Text()[0] {
		case '-':
			// add it in to the last txotx
			blocktxs[len(blocktxs)-1].txText += scanner.Text() + "\n"

		case '+':
			// add the whole line to inputBlob.  don't put a newline. do put
			// an x.
			blocktxs[len(blocktxs)-1].txText += scanner.Text() + "x"

			// chop up string
			parts := strings.Split(scanner.Text()[1:], ";")
			txid := parts[0]
			postsemicolon := parts[1]

			txoIndicators := strings.Split(postsemicolon, "z")
			numoutputs, err := strconv.Atoi(txoIndicators[0])
			if err != nil {
				return err
			}

			blocktxs[len(blocktxs)-1].outputtxid = txid
			blocktxs[len(blocktxs)-1].deathHeights = make([]uint32, numoutputs)

			// if len(blocktxs[len(blocktxs)-1].deathHeights) == 0 {
			//	fmt.Printf("txid\n", txid)
			//	panic("ded")
			// }
			// actually don't bother with unspendables, just look em up and they
			// won't be there.  Whatever.
			/*
				// detect unspendables & don't look for when they're spent
				unspendable := make(map[int]bool)
				// I think this is overkill as there's only ever one unspendable
				// output per tx.  but just in case. get em all.
				if len(txoIndicators) > 1 {
					unspendables := txoIndicators[1:]
					for _, zstring := range unspendables {
						n, err := strconv.Atoi(zstring)
						if err != nil {
							return err
						}
						unspendable[n] = true
					}
				}
			*/

			// done with this txotx, make the next one and append
			blocktxs = append(blocktxs, new(txotx))

		case 'h':
			// we started a tx but shouldn't have
			blocktxs = blocktxs[:len(blocktxs)-1]

			// call function to make all the db lookups and find deathheights
			// that part is in parallel.
			lookupBlock(blocktxs, lvdb)

			// write filled in txotx slice
			for _, tx := range blocktxs {
				// the txTest has all the inputs, and the output, and an x.
				// we just have to stick the numbers and commas on here.
				txstring := tx.txText
				for _, deathheight := range tx.deathHeights {
					if deathheight == 0 {
						txstring += "s,"
					} else {
						txstring += fmt.Sprintf("%d,", deathheight-height)
					}
				}

				_, err = ttlfile.WriteString(txstring + "\n")
				if err != nil {
					return err
				}
			}

			_, err = ttlfile.WriteString(scanner.Text() + "\n")
			if err != nil {
				return err
			}
			fmt.Printf("done with height %d\n", height)

			height++

			// start next block
			// wipe all block txs
			blocktxs = []*txotx{new(txotx)}

		default:
			panic("unknown string")
		}

	}
	return nil
}

func runTxo() error {

	//	 skip first pass -- we already have that file

	err := firstPass()
	if err != nil {
		return err
	}

	fmt.Printf("start 2nd pass \n")

	//	err := secondPass()
	//	if err != nil {
	//		return err
	//	}

	return nil
}
