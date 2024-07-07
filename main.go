package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	rnd "math/rand"
	"time"

	"github.com/ltcmweb/ltcd/chaincfg"
	"github.com/ltcmweb/ltcd/chaincfg/chainhash"
	"github.com/ltcmweb/ltcd/ltcutil"
	"github.com/ltcmweb/ltcd/ltcutil/mweb"
	"github.com/ltcmweb/ltcd/ltcutil/mweb/mw"
	"github.com/ltcmweb/ltcd/wire"
	"github.com/ltcmweb/neutrino"
	"github.com/ltcsuite/ltcwallet/walletdb"
	_ "github.com/ltcsuite/ltcwallet/walletdb/bdb"
)

var (
	db    walletdb.DB
	cs    *neutrino.ChainService
	keys  *mweb.Keychain
	coins = map[chainhash.Hash]*mweb.Coin{}
)

func main() {
	var err error
	defer func() {
		if err != nil {
			fmt.Println(err)
		}
	}()
	db, err = walletdb.Create("bdb", "neutrino.db", true, time.Minute)
	if err != nil {
		return
	}
	cs, err = neutrino.NewChainService(neutrino.Config{
		Database:    db,
		ChainParams: chaincfg.MainNetParams,
	})
	if err != nil {
		return
	}
	if err = loadKeychain(); err != nil {
		return
	}
	addr := ltcutil.NewAddressMweb(keys.Address(0), &chaincfg.MainNetParams)
	fmt.Println("Address =", addr.String())
	if err = fetchCoins(); err != nil {
		return
	}
	var sumCoins uint64
	for _, coin := range coins {
		sumCoins += coin.Value
	}
	fmt.Println("Total =", sumCoins, "lits")
	cs.RegisterMwebUtxosCallback(utxoHandler)
	if err = cs.Start(); err != nil {
		return
	}
	for height := uint32(0); ; <-time.After(2 * time.Second) {
		_, height2, err := cs.BlockHeaders.ChainTip()
		if err != nil {
			return
		}
		if height2 > height {
			fmt.Println("Syncing height", height2)
			height = height2
		}
	}
}

func loadKeychain() error {
	var (
		bucketKey      = []byte("mweb-keys")
		scanSecretKey  = []byte("scan-secret")
		spendSecretKey = []byte("spend-secret")
	)
	return walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(bucketKey)
		if err != nil {
			return err
		}
		scanSecret := bytes.Clone(bucket.Get(scanSecretKey))
		if scanSecret == nil {
			scanSecret = make([]byte, 32)
			rand.Read(scanSecret)
			err = bucket.Put(scanSecretKey, scanSecret)
			if err != nil {
				return err
			}
		}
		spendSecret := bytes.Clone(bucket.Get(spendSecretKey))
		if spendSecret == nil {
			spendSecret = make([]byte, 32)
			rand.Read(spendSecret)
			err = bucket.Put(spendSecretKey, spendSecret)
			if err != nil {
				return err
			}
		}
		keys = &mweb.Keychain{
			Scan:  (*mw.SecretKey)(scanSecret),
			Spend: (*mw.SecretKey)(spendSecret),
		}
		return nil
	})
}

func fetchCoins() error {
	lfs, err := cs.MwebCoinDB.GetLeafset()
	if err != nil {
		return err
	}
	var leaves []uint64
	for leaf := uint64(0); leaf < lfs.Size; leaf++ {
		if lfs.Contains(leaf) {
			leaves = append(leaves, leaf)
		}
		if len(leaves) == 1000 || leaf == lfs.Size-1 {
			utxos, err := cs.MwebCoinDB.FetchLeaves(leaves)
			if err != nil {
				return err
			}
			for _, utxo := range utxos {
				coin, err := mweb.RewindOutput(utxo.Output, keys.Scan)
				if err == nil {
					coin.CalculateOutputKey(keys.SpendKey(0))
					coins[*utxo.OutputId] = coin
				}
			}
			leaves = leaves[:0]
		}
	}
	return err
}

var (
	lastHeight uint32
	sent       bool
)

func utxoHandler(lfs *mweb.Leafset, utxos []*wire.MwebNetUtxo) {
	if lfs != nil && lfs.Height > lastHeight {
		lastHeight = lfs.Height
		sent = false
	}
	for _, utxo := range utxos {
		if utxo.Height == 0 {
			fmt.Println("Output in mempool", hex.EncodeToString(utxo.OutputId[:]))
			if !sent {
				if err := send(); err != nil {
					fmt.Println(err)
				} else {
					sent = true
				}
			}
		}
	}
}

func send() error {
	const fee = 3900
	selected := map[chainhash.Hash]*mweb.Coin{}
	var sumCoins uint64
	for sumCoins < fee {
		if len(selected) == len(coins) {
			return errors.New("balance too low")
		}
		i := rnd.Intn(len(coins))
		for _, coin := range coins {
			if i == 0 {
				if selected[*coin.OutputId] == nil {
					selected[*coin.OutputId] = coin
					sumCoins += coin.Value
				}
				break
			}
			i--
		}
	}
	sumCoins -= fee
	var inputs []*mweb.Coin
	for _, coin := range selected {
		inputs = append(inputs, coin)
	}
	mwebTx, newCoins, err := mweb.NewTransaction(inputs,
		[]*mweb.Recipient{
			{Value: sumCoins / 2, Address: keys.Address(0)},
			{Value: (sumCoins + 1) / 2, Address: keys.Address(0)},
		}, fee, 0, nil, nil)
	if err != nil {
		return err
	}
	tx := &wire.MsgTx{Version: 2, Mweb: mwebTx}
	if err = cs.SendTransaction(tx); err != nil {
		return err
	}
	for _, coin := range inputs {
		delete(coins, *coin.OutputId)
	}
	for _, coin := range newCoins {
		coins[*coin.OutputId] = coin
	}
	fmt.Println("Sent", tx.TxHash())
	fmt.Println("Inputs:")
	for _, coin := range inputs {
		fmt.Println("Value =", coin.Value, "lits, Output ID =",
			hex.EncodeToString(coin.OutputId[:]))
	}
	fmt.Println("Outputs:")
	for _, coin := range newCoins {
		fmt.Println("Value =", coin.Value, "lits, Output ID =",
			hex.EncodeToString(coin.OutputId[:]))
	}
	return nil
}
