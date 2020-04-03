package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/oguzbilgic/fpd"
	"github.com/wavesplatform/gowaves/pkg/client"
	"github.com/wavesplatform/gowaves/pkg/crypto"
	"github.com/wavesplatform/gowaves/pkg/proto"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	waves                = 100000000
	defaultScheme        = "http"
	standardFee   uint64 = 100000
)

var (
	version              = "v0.0.0"
	interruptSignals     = []os.Signal{os.Interrupt}
	errInvalidParameters = errors.New("invalid parameters")
	errUserTermination   = errors.New("user termination")
	errFailure           = errors.New("operation failure")
	na                   = proto.OptionalAsset{}
)

func main() {
	err := run()
	if err != nil {
		switch err {
		case errInvalidParameters:
			showUsage()
			os.Exit(2)
		case errUserTermination:
			os.Exit(130)
		case errFailure:
			os.Exit(70)
		default:
			os.Exit(1)
		}
	}
}

func run() error {
	var (
		nodeURL             string
		generatingAccountSK string
		lessorSK            string
		lessorPK            string
		irreducibleBalance  int64
		dryRun              bool
		testRun             bool
		showHelp            bool
		showVersion         bool
	)
	flag.StringVar(&nodeURL, "node-api", "http://localhost:6869", "Node's REST API URL")
	flag.StringVar(&generatingAccountSK, "generating-sk", "", "Base58 encoded private key of generating account")
	flag.StringVar(&lessorSK, "lessor-sk", "", "Base58 encoded private key of lessor")
	flag.StringVar(&lessorPK, "lessor-pk", "", "Base58 encoded lessor's public key")
	flag.Int64Var(&irreducibleBalance, "irreducible-balance", 0, "Irreducible balance on generating account in WAVELETS")
	flag.BoolVar(&dryRun, "dry-run", false, "Test execution without creating real transactions on blockchain")
	flag.BoolVar(&testRun, "test-run", false, "Test execution with limited available balance of 1 WAVES")
	flag.BoolVar(&showHelp, "help", false, "Show usage information and exit")
	flag.BoolVar(&showVersion, "version", false, "Print version information and quit")
	flag.Parse()

	if showHelp {
		showUsage()
		return nil
	}
	if showVersion {
		fmt.Printf("Waves Automatic Lessor %s\n", version)
		return nil
	}
	if nodeURL == "" || len(strings.Fields(nodeURL)) > 1 {
		log.Printf("[ERROR] Invalid node's URL '%s'", nodeURL)
		return errInvalidParameters
	}
	if generatingAccountSK == "" || len(strings.Fields(generatingAccountSK)) > 1 {
		log.Printf("[ERROR] Invalid generating account private key '%s'", generatingAccountSK)
		return errInvalidParameters
	}
	if lessorSK == "" || len(strings.Fields(lessorSK)) > 1 {
		log.Printf("[ERROR] Invalid lessor private key '%s'", lessorSK)
		return errInvalidParameters
	}
	if lessorPK == "" || len(strings.Fields(lessorPK)) > 1 {
		log.Print("[INFO] No different lessor public key is given")
	}
	if irreducibleBalance < 0 {
		log.Printf("[ERROR] Invalid irreducible balance value '%d'", irreducibleBalance)
		return errInvalidParameters
	}
	if irreducibleBalance > 0 {
		log.Printf("[INFO] Generating account irreducible balance set to %s", format(uint64(irreducibleBalance)))
	}
	if testRun {
		log.Printf("[INFO] TEST-RUN: Available balance will be limited to %s", format(waves))
	}
	if dryRun {
		log.Print("[INFO] DRY-RUN: No actual transactions will be created")
	}

	ctx := interruptListener()

	// 1. Check connection to node's API
	cl, err := nodeClient(ctx, nodeURL)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to connect to node at '%s': %v", nodeURL, err)
		return errFailure
	}
	log.Printf("[INFO] Successfully connected to '%s'", cl.GetOptions().BaseUrl)

	// 2. Acquire the network scheme from genesis block
	scheme, err := getScheme(ctx, cl)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to aquire blockchain scheme: %v", err)
		return errFailure
	}
	log.Printf("[INFO] Blockchain scheme: %s", string(scheme))

	// 3. Generate public keys and addresses from given private keys
	gSK, gPK, gAddr, err := parseSK(scheme, generatingAccountSK)
	if err != nil {
		log.Printf("[ERROR] Failed to parse generating private key: %v", err)
		return errFailure
	}
	log.Printf("[INFO] Generating address: %s", gAddr.String())
	lSK, lPK, lAddr, err := parseSK(scheme, lessorSK)
	if err != nil {
		log.Printf("[ERROR] Failed to parse lessor private key: %v", err)
		return errFailure
	}
	if lessorPK != "" {
		lPK, err = crypto.NewPublicKeyFromBase58(lessorPK)
		if err != nil {
			log.Printf("[ERROR] Failed to parse additional lessor public key: %v", err)
			return errFailure
		}
		lAddr, err = proto.NewAddressFromPublicKey(scheme, lPK)
		if err != nil {
			log.Printf("[ERROR] Failed to parce lessor address: %v", err)
			return errFailure
		}
	}
	log.Printf("[INFO] Lessor public key: %s", lPK.String())
	log.Printf("[INFO] Lessor address: %s", lAddr.String())

	// 4. Check WAVES balance on generating address
	balance, err := getWavesBalance(ctx, cl, gAddr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to get generator WAVES balance: %v", err)
		return errFailure
	}
	log.Printf("[INFO] Balance of generation account '%s': %s", gAddr.String(), format(balance))
	if irreducibleBalance > 0 {
		b := int64(balance) - irreducibleBalance
		if b > 0 {
			balance = uint64(b)
		} else {
			balance = 0
		}
	}
	if balance <= standardFee {
		log.Print("[ERROR] Not enough balance")
		return errFailure
	}
	if balance > waves && testRun {
		balance = waves
	}
	log.Printf("[INFO] Available balance: %s", format(balance))

	// 5. Create transfer transaction to lessor account
	rcp := proto.NewRecipientFromAddress(lAddr)
	transferExtraFee, err := getExtraFee(ctx, cl, gAddr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to check extra fee on account '%s': %v", lAddr.String(), err)
		return errFailure
	}
	if transferExtraFee != 0 {
		log.Printf("[INFO] Extra fee on transfer: %s", format(transferExtraFee))
	} else {
		log.Print("[INFO] No extra fee on transfer")
	}
	fee := standardFee + transferExtraFee
	amount := balance - fee
	transfer := proto.NewUnsignedTransferV2(gPK, na, na, timestamp(), amount, fee, rcp, "")
	err = transfer.Sign(gSK)
	if err != nil {
		log.Printf("[ERROR] Failed to sign transfer transaction: %v", err)
		return errFailure
	}
	if dryRun {
		b, err := json.Marshal(transfer)
		if err != nil {
			log.Printf("[ERROR] Failed to make transaction json: %v", err)
			return errFailure
		}
		log.Printf("[INFO] Transfer transaction:\n%s", string(b))
	} else {
		log.Printf("[INFO] Transfer transaction ID: %s", transfer.ID.String())
		err = broadcast(ctx, cl, transfer)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return errUserTermination
			}
			log.Printf("[ERROR] Failed to broadcast transfer transaction: %v", err)
			return errFailure
		}
		err = track(ctx, cl, *transfer.ID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return errUserTermination
			}
			log.Printf("[ERROR] Failed to track transfer transaction: %v", err)
			return errFailure
		}
	}

	// 6. Create leasing transaction to generating account
	rcp = proto.NewRecipientFromAddress(gAddr)
	leaseExtraFee, err := getExtraFee(ctx, cl, lAddr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to check extra fee on account '%s': %v", lAddr.String(), err)
		return errFailure
	}
	if leaseExtraFee != 0 {
		log.Printf("[INFO] Extra fee on lease: %s", format(leaseExtraFee))
	} else {
		log.Print("[INFO] No extra fee on lease")
	}
	fee = standardFee + leaseExtraFee
	amount = amount - fee
	lease := proto.NewUnsignedLeaseV2(lPK, rcp, amount, fee, timestamp())
	err = lease.Sign(lSK)
	if err != nil {
		log.Printf("[ERROR] Failed to sign lease transaction: %v", err)
		return errFailure
	}
	if dryRun {
		b, err := json.Marshal(lease)
		if err != nil {
			log.Printf("[ERROR] Failed to make transaction json: %v", err)
			return errFailure
		}
		log.Printf("[INFO] Lease transaction:\n%s", string(b))
	} else {
		log.Printf("[INFO] Lease transaction ID: %s", lease.ID.String())
		err = broadcast(ctx, cl, lease)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return errUserTermination
			}
			log.Printf("[ERROR] Failed to broadcast lease transaction: %v", err)
			return errFailure
		}
		err = track(ctx, cl, *lease.ID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return errUserTermination
			}
			log.Printf("[ERROR] Failed to track lease transaction: %v", err)
			return errFailure
		}
	}
	log.Print("[INFO] OK")
	return nil
}

func broadcast(ctx context.Context, cl *client.Client, tx proto.Transaction) error {
	_, err := cl.Transactions.Broadcast(ctx, tx)
	return err
}

func track(ctx context.Context, cl *client.Client, id crypto.Digest) error {
	log.Printf("[INFO] Waiting for transaction '%s' on blockchain...", id.String())
	for {
		_, rsp, err := cl.Transactions.Info(ctx, id)
		if errors.Is(err, context.Canceled) {
			return err
		}
		if rsp.StatusCode == http.StatusOK {
			return nil
		}
		time.Sleep(time.Second)
	}
}

func timestamp() uint64 {
	return uint64(time.Now().UnixNano()) / 1000000
}

func format(amount uint64) string {
	da := fpd.New(int64(amount), -8)
	return fmt.Sprintf("%s WAVES", da.FormattedString())
}

func getWavesBalance(ctx context.Context, cl *client.Client, addr proto.Address) (uint64, error) {
	ab, _, err := cl.Addresses.Balance(ctx, addr)
	if err != nil {
		return 0, err
	}
	return ab.Balance, nil
}

func getExtraFee(ctx context.Context, cl *client.Client, addr proto.Address) (uint64, error) {
	r, _, err := cl.Addresses.ScriptInfo(ctx, addr)
	if err != nil {
		return 0, err
	}
	return r.ExtraFee, nil
}

func nodeClient(ctx context.Context, s string) (*client.Client, error) {
	var u *url.URL
	var err error
	if strings.Contains(s, "//") {
		u, err = url.Parse(s)
	} else {
		u, err = url.Parse("//" + s)
	}
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" {
		u.Scheme = defaultScheme
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme '%s'", u.Scheme)
	}
	cl, err := client.NewClient(client.Options{BaseUrl: u.String(), Client: &http.Client{}})
	if err != nil {
		return nil, err
	}
	_, _, err = cl.Blocks.Height(ctx)
	if err != nil {
		return nil, err
	}
	return cl, nil
}

func getScheme(ctx context.Context, cl *client.Client) (proto.Scheme, error) {
	b, _, err := cl.Blocks.Last(ctx)
	if err != nil {
		return 0, err
	}
	return b.Generator.Bytes()[1], nil
}

func showUsage() {
	_, _ = fmt.Fprintf(os.Stderr, "\nUsage of Waves Automatic Lessor %s\n", version)
	flag.PrintDefaults()
}

func parseSK(scheme proto.Scheme, s string) (crypto.SecretKey, crypto.PublicKey, proto.Address, error) {
	sk, err := crypto.NewSecretKeyFromBase58(s)
	if err != nil {
		return crypto.SecretKey{}, crypto.PublicKey{}, proto.Address{}, err
	}
	pk := crypto.GeneratePublicKey(sk)
	address, err := proto.NewAddressFromPublicKey(scheme, pk)
	if err != nil {
		return crypto.SecretKey{}, crypto.PublicKey{}, proto.Address{}, err
	}
	return sk, pk, address, nil
}
