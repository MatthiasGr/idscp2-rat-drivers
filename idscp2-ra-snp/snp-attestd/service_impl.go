package snp_attestd

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	ar "github.com/industrial-data-space/idscp2-rat-drivers/idscp2-ra-snp/snp-attestd/attestation_report"
	log "github.com/industrial-data-space/idscp2-rat-drivers/idscp2-ra-snp/snp-attestd/logger"
	"github.com/industrial-data-space/idscp2-rat-drivers/idscp2-ra-snp/snp-attestd/policy"
	pb "github.com/industrial-data-space/idscp2-rat-drivers/idscp2-ra-snp/snp-attestd/snp_attestd_service"
)

var (
	errServer = errors.New("internal server error")
)

type AttestdServiceImpl struct {
	config Config
	dev    *SnpDevice

	pb.UnimplementedSnpAttestdServiceServer
}

func NewAttestdServiceImpl(config Config) (*AttestdServiceImpl, error) {
	var dev *SnpDevice
	var err error

	if !config.VerifyOnly {
		dev, err = OpenSnpDevice(config.SevDevice)
		if err != nil {
			return nil, err
		}
	}

	service := AttestdServiceImpl{
		config: config,
		dev:    dev,
	}

	return &service, nil
}

func (s *AttestdServiceImpl) getVcekCertPath(report ar.AttestationReport) (string, error) {
	// Each certificate is identified by the chip id and reported TCB value of the system.
	// Both values can be found in the attestation report
	// VCEK certificates are stored at `${config.CacheDir}/${SHA-1(report.ChipId | report.ReportedTcb)}`
	hash := sha1.New()
	if err := binary.Write(hash, binary.LittleEndian, &report.ChipId); err != nil {
		return "", fmt.Errorf("could not extend hash value: %w", err)
	}
	if err := binary.Write(hash, binary.LittleEndian, &report.ReportedTcb); err != nil {
		return "", fmt.Errorf("could not extend hash value: %w", err)
	}

	var pathBuilder strings.Builder
	// The errors of strings.Builder are only here for interface compatibility and can sefely be ignored
	pathBuilder.WriteString(s.config.CacheDir)
	pathBuilder.WriteRune(os.PathSeparator)
	pathBuilder.WriteString(hex.EncodeToString(hash.Sum(nil)))
	pathBuilder.WriteString(".crt")

	return pathBuilder.String(), nil
}

func (s *AttestdServiceImpl) getVcekCert(report ar.AttestationReport) ([]byte, error) {
	filePath, err := s.getVcekCertPath(report)
	if err != nil {
		return []byte{}, fmt.Errorf("could not determine the VCEK certificate's location: %w", err)
	}

	_, err = os.Stat(filePath)
	if err != nil {
		// If the file does not exist, we can fetch it
		// If any other error occurrs, we complain
		if !errors.Is(err, fs.ErrNotExist) {
			return []byte{}, fmt.Errorf("could not stat the cached certificate: %w", err)
		}

		// Create the vcek cache directory, if it does not exist
		if err := os.MkdirAll(s.config.CacheDir, 0755); err != nil {
			log.Debug("VCEK cache dir does not exist at %s. Creating...", s.config.CacheDir)
			return []byte{}, fmt.Errorf("the VCEK cache dir does not exist and could not be created: %w", err)
		}

		log.Debug("Fetching VCEK certificate from AMD KDC")
		certData, err := FetchVcekCertForReport(report)
		if err != nil {
			return []byte{}, fmt.Errorf("could not fetch VCEK certificate: %w", err)
		}

		// Write certificate to disk
		// If this fails, we can continue execution
		// Therefore we only complain to log and do not return an error
		if err := os.WriteFile(filePath, certData, 0755); err != nil {
			log.Warn("could not save VCEK certificate to cache: %v", err)
		}

		return certData, nil
	}

	certData, err := os.ReadFile(filePath)
	if err != nil {
		return []byte{}, fmt.Errorf("error reading VCEK certificate from file: %w", err)
	}

	log.Debug("Fetching VCEK from cache")
	return certData, nil
}

func (s *AttestdServiceImpl) loadCertChain() (ask *x509.Certificate, ark *x509.Certificate, err error) {
	askPath := path.Join(s.config.CacheDir, "ask.crt")
	arkPath := path.Join(s.config.CacheDir, "ark.crt")

	_, err = os.Stat(askPath)
	if err != nil {
		err = fmt.Errorf("could not stat the ASK's certificate file: %w", err)
		return
	}

	_, err = os.Stat(arkPath)
	if err != nil {
		err = fmt.Errorf("could not stat the ARK's certificate file: %w", err)
		return
	}

	loadCert := func(path string) (*x509.Certificate, error) {
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("error reading from file: %w", err)
		}

		cert, err := x509.ParseCertificate(contents)
		if err != nil {
			return nil, fmt.Errorf("could not decode certificate: %w", err)
		}

		return cert, nil
	}

	ask, err = loadCert(askPath)
	if err != nil {
		err = fmt.Errorf("could not load the ASK certificate: %w", err)
		return
	}

	ark, err = loadCert(arkPath)
	if err != nil {
		err = fmt.Errorf("could not load the ARK certificate: %w", err)
		return
	}

	return
}

// Implementation of the grpc interface

func (s *AttestdServiceImpl) GetReport(ctx context.Context, reportRequest *pb.ReportRequest) (*pb.ReportResponse, error) {
	if s.config.VerifyOnly {
		log.Debug("Got report request while in verify only mode. Ignoring.")
		return nil, fmt.Errorf("the service is in verify only mode and cannot provide attestation reports")
	}

	if len(reportRequest.ReportData) > 64 {
		log.Debug("Got a report request with %d bytes of report data. Refusing.", len(reportRequest.ReportData))
		return nil, fmt.Errorf("expected at most 64 bytes of report data, got %d bytes", len(reportRequest.ReportData))
	}
	if log.LogLevel >= log.LogDebug {
		log.Debug("Got a report request with report data %s", hex.EncodeToString(reportRequest.ReportData))
	}

	report, err := s.dev.GetReport(reportRequest.ReportData)
	if err != nil {
		log.Err("Error retreiving report from the SEV firmware: %v", err)
		return nil, errServer
	}

	var vcekCert []byte
	if reportRequest.IncludeVcekCert {
		vcekCert, err = s.getVcekCert(report)
		if err != nil {
			log.Err("Could not fetch vcek certificate: %v", err)
			return nil, errServer
		}
	}

	reportBuffer := new(bytes.Buffer)
	if err := binary.Write(reportBuffer, binary.LittleEndian, &report); err != nil {
		log.Err("Could not encode attestation report: %v", err)
		return nil, errServer
	}

	response := pb.ReportResponse{
		Report:   reportBuffer.Bytes(),
		VcekCert: vcekCert,
	}

	return &response, nil
}

func (s *AttestdServiceImpl) VerifyReport(ctx context.Context, verifyRequest *pb.VerifyRequest) (*pb.VerifyResponse, error) {
	log.Debug("Got Verify Request")
	if log.LogLevel >= log.LogTrace {
		log.Trace("Policy: %s", verifyRequest.Policies)
	}

	var report ar.AttestationReport
	reportBuf := bytes.NewReader(verifyRequest.Report)
	binary.Read(reportBuf, binary.LittleEndian, &report)

	ask, ark, err := s.loadCertChain()
	if err != nil {
		log.Err("Could not load the VCEK certificate chain: %v", err)
		return nil, errServer
	}

	// Step one: Verify that the VCEK is signed by AMD

	var vcekBytes []byte

	if len(verifyRequest.VcekCert) != 0 {
		vcekBytes = verifyRequest.VcekCert
	} else {
		vcekBytes, err = FetchVcekCertForReport(report)
		if err != nil {
			log.Err("Could not fetch VCEK certificate: %v", err)
			return nil, errServer
		}
	}

	vcek, err := x509.ParseCertificate(vcekBytes)
	if err != nil {
		log.Err("Could not decode the VCEK certificate: %v", err)
		return nil, errServer
	}

	verifyOptions := x509.VerifyOptions{}
	verifyOptions.Roots = x509.NewCertPool()
	verifyOptions.Roots.AddCert(ark)
	verifyOptions.Intermediates = x509.NewCertPool()
	verifyOptions.Intermediates.AddCert(ask)

	chains, err := vcek.Verify(verifyOptions)
	if err != nil {
		log.Err("Error during certificate verification: %v", err)
		return nil, errServer
	}

	// For verification to be successful, there must be exactly one certificate chain
	// vcek -> ask -> ark
	if len(chains) != 1 || len(chains[0]) != 3 {
		log.Debug("Report verification failed as the VCEK certificate's signature could not be verified.")
		return &pb.VerifyResponse{}, nil
	}

	if !VerifyVcekCertificateExtensions(vcek, report) {
		log.Debug("Report verification failed as the VCEK certificate's X.509 extensions did not match the report.")
		return &pb.VerifyResponse{}, nil
	}

	// Step two: Verify the report signature

	ok, err := report.VerifySignature(verifyRequest.Report, vcek)
	if err != nil {
		log.Err("Error trying to verify the report's signature: %v", err)
		return nil, errServer
	}

	if !ok {
		log.Debug("Report verification failed as the report's siganture could not be verified.")
		return &pb.VerifyResponse{}, nil
	}

	// Step three: Do policy verification

	policies, err := policy.ParsePolicies([]byte(verifyRequest.Policies))
	if err != nil {
		log.Err("Could not parse policies: %v", err)
		// Since this is most likely a caller error (e.g. malformed json), we do not return the generic server error
		return &pb.VerifyResponse{}, fmt.Errorf("could not parse policies: %v", err)
	}

	ok, reasons, err := policy.CheckPolicies(policies, &report)
	if err != nil {
		log.Err("Error during policy checking: %v", err)
		return &pb.VerifyResponse{}, errServer
	}

	if !ok {
		log.Debug("Report verification failed as the report did not pass the policy check: %v", reasons)
		return &pb.VerifyResponse{}, nil
	}

	log.Debug("Report verification succeeded")
	response := pb.VerifyResponse{
		Ok: true,
	}
	return &response, nil
}