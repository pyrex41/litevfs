package main

import (
    "fmt"
    "os"
    "regexp"
)

// Test CURVE key validation (simplified)
func validateCurveKey(key string) bool {
    if len(key) != 40 {
        return false
    }
    
    // Check Z85 character set
    validZ85 := regexp.MustCompile(`^[0-9a-zA-Z\.\-:+=\^!/\*\?\&<>\(\)\[\]{}@%\$#]+$`)
    return validZ85.MatchString(key)
}

func main() {
    // Test keys from ZMQ documentation
    testServerPublic := "rq:rM>}U?@Lns47E1%kR.o@n%FcmmsL/@{H8]yf7"
    testServerSecret := "JTKVSB%%)wK0E.X)V>+}o?pNmC{O&4W4b!Ni{Lh6"
    
    fmt.Printf("Testing CURVE key validation...\n")
    
    if validateCurveKey(testServerPublic) {
        fmt.Printf("✅ Test server public key is valid\n")
    } else {
        fmt.Printf("❌ Test server public key is invalid\n")
        os.Exit(1)
    }
    
    if validateCurveKey(testServerSecret) {
        fmt.Printf("✅ Test server secret key is valid\n")
    } else {
        fmt.Printf("❌ Test server secret key is invalid\n")
        os.Exit(1)
    }
    
    // Test invalid keys
    if !validateCurveKey("InvalidKey") {
        fmt.Printf("✅ Invalid key properly rejected\n")
    } else {
        fmt.Printf("❌ Invalid key not rejected\n")
        os.Exit(1)
    }
    
    if !validateCurveKey("") {
        fmt.Printf("✅ Empty key properly rejected\n")
    } else {
        fmt.Printf("❌ Empty key not rejected\n")
        os.Exit(1)
    }
    
    fmt.Printf("🔐 All CURVE key validation tests passed!\n")
}
