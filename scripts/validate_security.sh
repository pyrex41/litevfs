#!/bin/bash

# 🔒 DISTSQLITE SECURITY VALIDATION SCRIPT
# This script validates that critical security vulnerabilities have been addressed

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Security check results
CRITICAL_ISSUES=0
HIGH_ISSUES=0
MEDIUM_ISSUES=0
PASSED_CHECKS=0

echo -e "${BLUE}🔒 DistSQLite Security Validation${NC}"
echo "========================================"
echo "Checking for critical security vulnerabilities..."
echo ""

# Function to report security issue
report_issue() {
    local severity=$1
    local title=$2
    local description=$3
    local recommendation=$4
    
    case $severity in
        "CRITICAL")
            echo -e "${RED}🔴 CRITICAL: $title${NC}"
            ((CRITICAL_ISSUES++))
            ;;
        "HIGH")
            echo -e "${YELLOW}🟡 HIGH: $title${NC}"
            ((HIGH_ISSUES++))
            ;;
        "MEDIUM")
            echo -e "${YELLOW}🟠 MEDIUM: $title${NC}"
            ((MEDIUM_ISSUES++))
            ;;
    esac
    
    echo "   Description: $description"
    echo "   Recommendation: $recommendation"
    echo ""
}

# Function to report passed check
report_pass() {
    local check_name=$1
    echo -e "${GREEN}✅ PASS: $check_name${NC}"
    ((PASSED_CHECKS++))
}

echo "🔍 Checking for SQL injection vulnerabilities..."

# Check 1: Verify HTTP SQL replay is disabled
if grep -q "SECURITY_BLOCK" pkg/replication/sql_replay.go 2>/dev/null; then
    report_pass "HTTP SQL replay endpoint is properly disabled"
else
    report_issue "CRITICAL" \
        "HTTP SQL replay endpoint not secured" \
        "The vulnerable HTTP SQL replay endpoint is still active" \
        "Disable the endpoint or add security blocks (see CRITICAL_SECURITY_FIXES.md)"
fi

# Check 2: Verify vulnerable SQL statement execution is disabled
if grep -q "disabled for security" pkg/replication/sql_replay.go 2>/dev/null; then
    report_pass "Vulnerable SQL statement execution is disabled"
else
    report_issue "CRITICAL" \
        "SQL injection vulnerability still present" \
        "The applySQLStatement function with SQL injection vulnerability is active" \
        "Disable SQL statement execution and use secure WAL replication"
fi

echo "🔐 Checking ZMQ security configuration..."

# Check 3: Verify secure WAL replication exists
if [ -f "pkg/replication/wal_zmq_secure.go" ]; then
    report_pass "Secure ZMQ WAL replication implementation exists"
else
    report_issue "HIGH" \
        "Secure WAL replication not implemented" \
        "No secure ZMQ-based WAL replication found" \
        "Implement secure binary WAL replication via ZMQ"
fi

# Check 4: Verify secure transport exists
if [ -f "pkg/zmq/secure_transport.go" ]; then
    report_pass "Secure ZMQ transport implementation exists"
else
    report_issue "HIGH" \
        "Secure ZMQ transport not implemented" \
        "No secure ZMQ transport with CURVE authentication found" \
        "Implement secure ZMQ transport with mandatory CURVE authentication"
fi

# Check 5: Check for CURVE authentication enforcement
if grep -q "CURVE authentication is required" pkg/zmq/* 2>/dev/null; then
    report_pass "CURVE authentication enforcement found"
else
    report_issue "HIGH" \
        "CURVE authentication not enforced" \
        "ZMQ transport does not enforce CURVE authentication" \
        "Add CURVE authentication validation in transport initialization"
fi

echo "📋 Checking configuration security..."

# Check 6: Verify secure production config exists
if [ -f "deploy/security/secure-production.yml" ]; then
    report_pass "Secure production configuration exists"
else
    report_issue "MEDIUM" \
        "Secure production configuration missing" \
        "No secure production configuration template found" \
        "Create secure production configuration with CURVE enabled"
fi

# Check 7: Check for security documentation
if [ -f "CRITICAL_SECURITY_FIXES.md" ]; then
    report_pass "Security documentation exists"
else
    report_issue "MEDIUM" \
        "Security documentation missing" \
        "No security fixes documentation found" \
        "Create security documentation with fix details"
fi

echo "🚫 Checking for disabled vulnerable components..."

# Check 8: Verify HTTP endpoints are secured or disabled
if grep -q "SQL replay disabled for security" pkg/replication/sql_replay.go 2>/dev/null; then
    report_pass "HTTP SQL replay properly disabled with security message"
else
    report_issue "CRITICAL" \
        "HTTP SQL replay not properly disabled" \
        "HTTP SQL replay endpoint does not show security disable message" \
        "Add clear security blocking message to HTTP endpoints"
fi

# Check 9: Check for authentication mechanisms
if [ -d "deploy/security" ] && grep -q "authentication.*required" deploy/security/* 2>/dev/null; then
    report_pass "Authentication requirements found in security config"
else
    report_issue "HIGH" \
        "Authentication not configured" \
        "No authentication requirements found in configurations" \
        "Add authentication requirements to all HTTP endpoints"
fi

# Check 10: Check for security headers configuration
if [ -d "deploy/security" ] && grep -q "X-Content-Type-Options" deploy/security/* 2>/dev/null; then
    report_pass "Security headers configured"
else
    report_issue "MEDIUM" \
        "Security headers not configured" \
        "HTTP security headers not found in configuration" \
        "Add security headers to HTTP configuration"
fi

echo "🔍 Checking for environment variable security..."

# Check 11: Verify environment variables are documented
if [ -d "deploy/security" ] && grep -q "ZMQ_SERVER_PUBLIC_KEY" deploy/security/* 2>/dev/null; then
    report_pass "ZMQ security keys documented in configuration"
else
    report_issue "HIGH" \
        "ZMQ security keys not documented" \
        "Environment variables for ZMQ CURVE keys not documented" \
        "Document required environment variables for CURVE authentication"
fi

# Check 12: Check for test key warnings
if grep -q "development only" pkg/zmq/* 2>/dev/null; then
    report_pass "Test key warnings present"
else
    report_issue "MEDIUM" \
        "Test key warnings missing" \
        "No warnings about test keys vs production keys" \
        "Add warnings when test keys are used"
fi

echo ""
echo "========================================"
echo -e "${BLUE}🔒 SECURITY VALIDATION SUMMARY${NC}"
echo "========================================"

echo -e "${GREEN}✅ Passed Checks: $PASSED_CHECKS${NC}"

if [ $CRITICAL_ISSUES -gt 0 ]; then
    echo -e "${RED}🔴 Critical Issues: $CRITICAL_ISSUES${NC}"
fi

if [ $HIGH_ISSUES -gt 0 ]; then
    echo -e "${YELLOW}🟡 High Priority Issues: $HIGH_ISSUES${NC}"
fi

if [ $MEDIUM_ISSUES -gt 0 ]; then
    echo -e "${YELLOW}🟠 Medium Priority Issues: $MEDIUM_ISSUES${NC}"
fi

echo ""

# Overall security status
if [ $CRITICAL_ISSUES -eq 0 ] && [ $HIGH_ISSUES -eq 0 ]; then
    echo -e "${GREEN}🎉 SECURITY STATUS: ACCEPTABLE FOR PRODUCTION${NC}"
    echo "All critical and high-priority security issues have been addressed."
    echo ""
    echo "Next steps:"
    echo "1. Generate production CURVE keypairs"
    echo "2. Set environment variables"
    echo "3. Deploy with secure configuration"
    echo "4. Test ZMQ CURVE authentication"
    exit 0
elif [ $CRITICAL_ISSUES -eq 0 ]; then
    echo -e "${YELLOW}⚠️  SECURITY STATUS: NEEDS ATTENTION${NC}"
    echo "No critical issues, but high-priority issues need to be addressed."
    echo ""
    echo "Action required:"
    echo "1. Address high-priority security issues"
    echo "2. Review medium-priority recommendations"
    exit 1
else
    echo -e "${RED}🚨 SECURITY STATUS: NOT READY FOR PRODUCTION${NC}"
    echo "CRITICAL security vulnerabilities must be fixed immediately!"
    echo ""
    echo "URGENT ACTION REQUIRED:"
    echo "1. Fix all critical security issues"
    echo "2. Disable vulnerable HTTP endpoints"
    echo "3. Enable ZMQ CURVE authentication"
    echo "4. Re-run this validation script"
    exit 2
fi