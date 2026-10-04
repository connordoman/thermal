package settings

import (
	"os"
	"strconv"
	"strings"
)

const (
	EnvEscposVendorId  = "ESCPOS_VENDOR_ID"
	EnvEscposProductId = "ESCPOS_PRODUCT_ID"
)

type PrinterConfig struct {
	VendorId  uint16
	ProductId uint16
}

func NewPrinterConfig() *PrinterConfig {
	return &PrinterConfig{}
}

// parseHexId reads a 16-bit hex ID from the environment, with or without a 0x prefix.
func parseHexId(key string) (uint64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	v = strings.TrimPrefix(strings.TrimPrefix(v, "0x"), "0X")
	return strconv.ParseUint(v, 16, 16)
}

func (p *PrinterConfig) Load() error {
	parsedVendor, err := parseHexId(EnvEscposVendorId)
	if err != nil {
		return configError("VendorId", "Failed to parse "+EnvEscposVendorId+": "+err.Error())
	}
	parsedProduct, err := parseHexId(EnvEscposProductId)
	if err != nil {
		return configError("ProductId", "Failed to parse "+EnvEscposProductId+": "+err.Error())
	}
	p.VendorId = uint16(parsedVendor)
	p.ProductId = uint16(parsedProduct)
	return nil
}

func (p *PrinterConfig) Validate() error {
	if p.VendorId == 0 {
		return configError("VendorId", "VendorId cannot be zero")
	}
	if p.ProductId == 0 {
		return configError("ProductId", "ProductId cannot be zero")
	}
	return nil
}
