package instance

import (
	"crypto/rsa"
	"io"
	"math/big"
	"os"
	"sync"
	"testing"
)

// Each pair of these 512-bit primes is a valid 1024-bit key. They were cut from
// keys rsa.GenerateKey made, so the moduli carry its size and spacing guarantees.
var testServingKeyPrimes = []string{
	"c1c0a098aa9a2f5f7718fb52735f03feccc1e53f2d313111eb5fd8962c9cd617a843a5983ca0d25c37fd7a9d326c792bcce766dc9f703fc9ff22a30cecd43ba3",
	"efa8c62d7f171510712d07ac5e9d9f0296072f3d167bcbf93596abcd24bde28d24d133e8e1c726735941d919da196c1d7bc5fcf20dd69f9ef50c0e474c6486ed",
	"c824c67522628c69ae5f68cefd1605caee222fb048b7474d50d1106a5a2336ae95566d3f1c5de43502581b93ee154d9da153afe8da6d7f98e004afe8dab58df5",
	"edeee5290f845adefedd3733e4b90d0bf7f4070e26423e04b72cfd0929c3e9a165c98fe1df8abd2ef1af75dccf4474f3a7fd2c231ca8b9702f2c6e4f178e79ef",
	"e2029b92f6644097ee0141872cce85368d7de0620a0787f7a9c3668a52493f5bda4289a83ce35c50a2928df97184f3c89ed06f9db1d932697d67e0571125310d",
	"c3e1f3f680fc588a7bed1ccfcf81ea1a29b6e99ef829dc28da5bf94a22e1d539a9feda0473c3a3715a59847afebe9f5afb23daf02dc448b47e8bce0fa3c8f789",
	"c5aaab2987500e3ebf474ebd7e4aefe4c5a5a8252fb0017c9a77341525bdf3f702b8f7b4847b4c1956b95a4c8de76ffa19d2f2da5ecff8e48f844f93496d598f",
	"fa6ad6f876926995e75c89800e2c2ba43bc41fae277f018cc02c108959461bd2bf8e4d1de991715b998e28dd4987d2550fdad3522adf05dd79471f1d8b13041f",
	"d4ee1bd5a9069409915c2cb9aa430dc6638341adc3dd94668909b0ce655d8079673373640b8dcee8bcc3072df55b0bb5c37d22b0a3ba8d92451b51195fc97e55",
	"e05b107795cbc3161ff61b2fecd66f0bbb7cfed9887f49bf4191111cf6bf6209e047bab8092b34e30b04c03ffd82b7340cc1020010241a1cc562c45658d3825f",
	"c442965560c122c51c5faa967bb1b1a036ce702f557914aae2801a93a5f95ea070f565166b246daa34ba11b3c0d6d662e5fe905f732a7f2a82d828c206160a97",
	"d3156843aa24d2893e95e9cd7452e2cad467c551b0aa933a5ca044a57543a0a090a9db0a390fc5cffedb0fd45295aa6a3ea2195c98a2631b775feabe555fcf5b",
	"ec6e7d01c33bcf56487a658fdc6aa85143e8e509d0e9cbb9c94655e0eff1596bcb6f3df4caead9d32b1d73f371dc02a4e5458aeb4e9f1bead9af11d138acd267",
	"cfdf648e965e15a0a745e47f3e8735e3e73feed77b68189a9876d41f2e13e188507db2f53ec7473eb01154f6bb24532db59cbc7548f9851cf541b9cbeae97b3f",
	"fe054cfd4e34b20dd6044d54c3e8aeb866ede9caab45cdd75ebbd044e8e4cdfebdedf829fc681f363d2557cbb7cf926e26ed11e5233b3974ed749ed5f6124bd9",
	"fc82cc3278ee006368f1f84973d87f557c3f07dce062a8decade3d27b5a1f77fff432adf55fac36e8b8ba56f8df2e36f29a773e61d0d375c96a93e84d323ecf7",
}

func TestMain(m *testing.M) {
	generateServingKey = newServingKeyPool(generateServingKey)
	os.Exit(m.Run())
}

// Hands out every prime pair once, so no two mints in a run share a key, then
// falls back to real generation. Other sizes always generate.
func newServingKeyPool(generate func(io.Reader, int) (*rsa.PrivateKey, error)) func(io.Reader, int) (*rsa.PrivateKey, error) {
	var mu sync.Mutex
	i, j := 0, 1
	return func(random io.Reader, bits int) (*rsa.PrivateKey, error) {
		if bits != testServingCertKeyBits {
			return generate(random, bits)
		}
		mu.Lock()
		if j >= len(testServingKeyPrimes) {
			mu.Unlock()
			return generate(random, bits)
		}
		p, q := mustParsePrime(testServingKeyPrimes[i]), mustParsePrime(testServingKeyPrimes[j])
		if j++; j == len(testServingKeyPrimes) {
			i++
			j = i + 1
		}
		mu.Unlock()
		return keyFromPrimes(p, q)
	}
}

func mustParsePrime(hex string) *big.Int {
	p, ok := new(big.Int).SetString(hex, 16)
	if !ok {
		panic("unparseable test prime " + hex)
	}
	return p
}

func keyFromPrimes(p, q *big.Int) (*rsa.PrivateKey, error) {
	const e = 65537
	one := big.NewInt(1)
	phi := new(big.Int).Mul(new(big.Int).Sub(p, one), new(big.Int).Sub(q, one))
	key := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{N: new(big.Int).Mul(p, q), E: e},
		D:         new(big.Int).ModInverse(big.NewInt(e), phi),
		Primes:    []*big.Int{p, q},
	}
	key.Precompute()
	if err := key.Validate(); err != nil {
		return nil, err
	}
	return key, nil
}
