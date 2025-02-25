// Copyright 2017 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package networkutils

import (
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	mocks_ip "github.com/aws/amazon-vpc-cni-k8s/pkg/ipwrapper/mocks"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/netlinkwrapper/mock_netlink"
	mock_netlinkwrapper "github.com/aws/amazon-vpc-cni-k8s/pkg/netlinkwrapper/mocks"
	mock_nswrapper "github.com/aws/amazon-vpc-cni-k8s/pkg/nswrapper/mocks"
)

const (
	testMAC1      = "01:23:45:67:89:a0"
	testMAC2      = "01:23:45:67:89:a1"
	testTable     = 10
	testeniIP     = "10.10.10.20"
	testeniSubnet = "10.10.0.0/16"
	// Default MTU of ENI and veth
	// defined in plugins/routed-eni/driver/driver.go, pkg/networkutils/network.go
	testMTU = 9001
)

var (
	_, testENINetIPNet, _ = net.ParseCIDR(testeniSubnet)
	testENINetIP          = net.ParseIP(testeniIP)
)

func setup(t *testing.T) (*gomock.Controller,
	*mock_netlinkwrapper.MockNetLink,
	*mocks_ip.MockIP,
	*mock_nswrapper.MockNS,
	*mockIptables) {
	ctrl := gomock.NewController(t)
	return ctrl,
		mock_netlinkwrapper.NewMockNetLink(ctrl),
		mocks_ip.NewMockIP(ctrl),
		mock_nswrapper.NewMockNS(ctrl),
		newMockIptables()
}

func TestSetupENINetwork(t *testing.T) {
	ctrl, mockNetLink, _, _, _ := setup(t)
	defer ctrl.Finish()

	hwAddr, err := net.ParseMAC(testMAC1)
	assert.NoError(t, err)

	mockLinkAttrs1 := &netlink.LinkAttrs{
		HardwareAddr: hwAddr,
	}
	hwAddr, err = net.ParseMAC(testMAC2)
	assert.NoError(t, err)

	mockLinkAttrs2 := &netlink.LinkAttrs{
		HardwareAddr: hwAddr,
	}

	lo := mock_netlink.NewMockLink(ctrl)
	eth1 := mock_netlink.NewMockLink(ctrl)
	// Emulate a delay attaching the ENI so a retry is necessary
	// First attempt gets one links
	firstlistSet := mockNetLink.EXPECT().LinkList().Return([]netlink.Link([]netlink.Link{lo}), nil)
	lo.EXPECT().Attrs().Return(mockLinkAttrs1)
	// Second attempt gets both links
	secondlistSet := mockNetLink.EXPECT().LinkList().Return([]netlink.Link([]netlink.Link{lo, eth1}), nil)
	lo.EXPECT().Attrs().Return(mockLinkAttrs1)
	eth1.EXPECT().Attrs().Return(mockLinkAttrs2)
	gomock.InOrder(firstlistSet, secondlistSet)

	mockNetLink.EXPECT().LinkSetMTU(gomock.Any(), testMTU).Return(nil)
	mockNetLink.EXPECT().LinkSetUp(gomock.Any()).Return(nil)

	// eth1's device
	eth1.EXPECT().Attrs().Return(mockLinkAttrs2)
	eth1.EXPECT().Attrs().Return(mockLinkAttrs2)

	// eth1's IP address
	testeniAddr := &net.IPNet{
		IP:   net.ParseIP(testeniIP),
		Mask: testENINetIPNet.Mask,
	}
	mockNetLink.EXPECT().AddrList(gomock.Any(), unix.AF_INET).Return([]netlink.Addr{}, nil)
	mockNetLink.EXPECT().AddrAdd(gomock.Any(), &netlink.Addr{IPNet: testeniAddr}).Return(nil)

	mockNetLink.EXPECT().RouteDel(gomock.Any())
	mockNetLink.EXPECT().RouteAdd(gomock.Any()).Return(nil)

	mockNetLink.EXPECT().RouteDel(gomock.Any())
	mockNetLink.EXPECT().RouteAdd(gomock.Any()).Return(nil)

	mockNetLink.EXPECT().RouteDel(gomock.Any()).Return(nil)

	err = setupENINetwork(testeniIP, testMAC2, testTable, testeniSubnet, mockNetLink, 0*time.Second, 0*time.Second, testMTU)
	assert.NoError(t, err)
}

func TestSetupENINetworkMACFail(t *testing.T) {
	ctrl, mockNetLink, _, _, _ := setup(t)
	defer ctrl.Finish()

	// Emulate a delay attaching the ENI so a retry is necessary
	// First attempt gets one links
	for i := 0; i < maxAttemptsLinkByMac; i++ {
		mockNetLink.EXPECT().LinkList().Return(nil, fmt.Errorf("simulated failure"))
	}

	err := setupENINetwork(testeniIP, testMAC2, testTable, testeniSubnet, mockNetLink, 0*time.Second, 0*time.Second, testMTU)
	assert.Errorf(t, err, "simulated failure")
}

func TestSetupENINetworkPrimary(t *testing.T) {
	ctrl, mockNetLink, _, _, _ := setup(t)
	defer ctrl.Finish()

	err := setupENINetwork(testeniIP, testMAC2, 0, testeniSubnet, mockNetLink, 0*time.Second, 0*time.Second, testMTU)
	assert.NoError(t, err)
}

func TestSetupHostNetworkNodePortDisabled(t *testing.T) {
	ctrl, mockNetLink, _, mockNS, mockIptables := setup(t)
	defer ctrl.Finish()

	ln := &linuxNetwork{
		mainENIMark: 0x80,

		netLink: mockNetLink,
		ns:      mockNS,
		newIptables: func() (iptablesIface, error) {
			return mockIptables, nil
		},
	}

	var hostRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&hostRule)
	mockNetLink.EXPECT().RuleDel(&hostRule)
	var mainENIRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&mainENIRule)
	mockNetLink.EXPECT().RuleDel(&mainENIRule)

	var vpcCIDRs []*string
	err := ln.SetupHostNetwork(testENINetIPNet, vpcCIDRs, "", &testENINetIP)
	assert.NoError(t, err)
}

func TestUpdateRuleListBySrc(t *testing.T) {
	ctrl, mockNetLink, _, _, _ := setup(t)
	defer ctrl.Finish()

	ln := &linuxNetwork{netLink: mockNetLink}

	origRule := netlink.Rule{
		Src:   testENINetIPNet,
		Table: testTable,
	}
	testCases := []struct {
		name               string
		oldRule            netlink.Rule
		requiresSNAT       bool
		toCIDRs            []string
		snatExclusionCIDRs []string
		ruleList           []netlink.Rule
		newRules           []netlink.Rule
		expDst             []*net.IPNet
		expTable           []int
	}{
		{
			"multiple destinations",
			origRule,
			true,
			[]string{"10.10.0.0/16", "10.11.0.0/16"},
			nil,
			[]netlink.Rule{origRule},
			make([]netlink.Rule, 2),
			make([]*net.IPNet, 2),
			[]int{origRule.Table, origRule.Table},
		},
		{
			"single destination",
			origRule,
			false,
			[]string{""},
			nil,
			[]netlink.Rule{origRule},
			make([]netlink.Rule, 1),
			make([]*net.IPNet, 1),
			[]int{origRule.Table},
		},
		{
			"SNAT exclusions",
			origRule,
			true,
			[]string{"10.10.0.0/16", "10.11.0.0/16"},
			[]string{"10.12.0.0/16", "10.13.0.0/16"},
			[]netlink.Rule{origRule},
			make([]netlink.Rule, 4),
			make([]*net.IPNet, 4),
			[]int{origRule.Table, origRule.Table, origRule.Table, origRule.Table},
		},
	}

	for _, tc := range testCases {
		ln.excludeSNATCIDRs = tc.snatExclusionCIDRs
		var newRuleSize int
		if tc.requiresSNAT {
			newRuleSize = len(tc.toCIDRs) + len(tc.snatExclusionCIDRs)
		} else {
			newRuleSize = 1
		}

		allCIDRs := append(tc.toCIDRs, tc.snatExclusionCIDRs...)
		for i := 0; i < newRuleSize; i++ {
			_, tc.expDst[i], _ = net.ParseCIDR(allCIDRs[i])
		}

		mockNetLink.EXPECT().RuleDel(&tc.oldRule)

		for i := 0; i < newRuleSize; i++ {
			mockNetLink.EXPECT().NewRule().Return(&tc.newRules[i])
			mockNetLink.EXPECT().RuleAdd(&tc.newRules[i])
		}

		err := ln.UpdateRuleListBySrc(tc.ruleList, *testENINetIPNet, tc.toCIDRs, tc.requiresSNAT)
		assert.NoError(t, err)

		for i := 0; i < newRuleSize; i++ {
			assert.Equal(t, tc.oldRule.Src, tc.newRules[i].Src, tc.name)
			assert.Equal(t, tc.expDst[i], tc.newRules[i].Dst, tc.name)
			assert.Equal(t, tc.expTable[i], tc.newRules[i].Table, tc.name)
		}
	}
}

func TestSetupHostNetworkNodePortEnabled(t *testing.T) {
	ctrl, mockNetLink, _, mockNS, mockIptables := setup(t)
	defer ctrl.Finish()

	var mockRPFilter mockFile
	ln := &linuxNetwork{
		useExternalSNAT:        true,
		nodePortSupportEnabled: true,
		mainENIMark:            defaultConnmark,

		netLink: mockNetLink,
		ns:      mockNS,
		newIptables: func() (iptablesIface, error) {
			return mockIptables, nil
		},
		openFile: func(name string, flag int, perm os.FileMode) (stringWriteCloser, error) {
			return &mockRPFilter, nil
		},
	}

	var hostRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&hostRule)
	mockNetLink.EXPECT().RuleDel(&hostRule)
	var mainENIRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&mainENIRule)
	mockNetLink.EXPECT().RuleDel(&mainENIRule)
	mockNetLink.EXPECT().RuleAdd(&mainENIRule)

	var vpcCIDRs []*string

	// loopback for primary device is a little bit hacky. But the test is stable and it should be
	// OK for test purpose.
	LoopBackMac := ""

	err := ln.SetupHostNetwork(testENINetIPNet, vpcCIDRs, LoopBackMac, &testENINetIP)
	assert.NoError(t, err)

	assert.Equal(t, map[string]map[string][][]string{
		"mangle": {
			"PREROUTING": [][]string{
				{
					"-m", "comment", "--comment", "AWS, primary ENI",
					"-i", "lo",
					"-m", "addrtype", "--dst-type", "LOCAL", "--limit-iface-in",
					"-j", "CONNMARK", "--set-mark", "0x80/0x80",
				},
				{
					"-m", "comment", "--comment", "AWS, primary ENI",
					"-i", "eni+", "-j", "CONNMARK", "--restore-mark", "--mask", "0x80",
				},
			},
		},
	}, mockIptables.dataplaneState)
	assert.Equal(t, mockFile{closed: true, data: "2"}, mockRPFilter)
}

func TestLoadMTUFromEnvTooLow(t *testing.T) {
	_ = os.Setenv(envMTU, "1")
	assert.Equal(t, GetEthernetMTU(), minimumMTU)
}

func TestLoadMTUFromEnv1500(t *testing.T) {
	_ = os.Setenv(envMTU, "1500")
	assert.Equal(t, GetEthernetMTU(), 1500)
}

func TestLoadMTUFromEnvTooHigh(t *testing.T) {
	_ = os.Setenv(envMTU, "65536")
	assert.Equal(t, GetEthernetMTU(), maximumMTU)
}

func TestLoadExcludeSNATCIDRsFromEnv(t *testing.T) {
	_ = os.Setenv(envExternalSNAT, "false")
	_ = os.Setenv(envExcludeSNATCIDRs, "10.12.0.0/16,10.13.0.0/16")

	expected := []string{"10.12.0.0/16", "10.13.0.0/16"}
	assert.Equal(t, getExcludeSNATCIDRs(), expected)
}

func TestSetupHostNetworkWithExcludeSNATCIDRs(t *testing.T) {
	ctrl, mockNetLink, _, mockNS, mockIptables := setup(t)
	defer ctrl.Finish()

	var mockRPFilter mockFile
	ln := &linuxNetwork{
		useExternalSNAT:        false,
		excludeSNATCIDRs:       []string{"10.12.0.0/16", "10.13.0.0/16"},
		nodePortSupportEnabled: true,
		mainENIMark:            defaultConnmark,

		netLink: mockNetLink,
		ns:      mockNS,
		newIptables: func() (iptablesIface, error) {
			return mockIptables, nil
		},
		openFile: func(name string, flag int, perm os.FileMode) (stringWriteCloser, error) {
			return &mockRPFilter, nil
		},
	}

	var hostRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&hostRule)
	mockNetLink.EXPECT().RuleDel(&hostRule)
	var mainENIRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&mainENIRule)
	mockNetLink.EXPECT().RuleDel(&mainENIRule)
	mockNetLink.EXPECT().RuleAdd(&mainENIRule)

	var vpcCIDRs []*string
	vpcCIDRs = []*string{aws.String("10.10.0.0/16"), aws.String("10.11.0.0/16")}
	err := ln.SetupHostNetwork(testENINetIPNet, vpcCIDRs, "", &testENINetIP)
	assert.NoError(t, err)
	assert.Equal(t,
		map[string]map[string][][]string{
			"nat": {
				"AWS-SNAT-CHAIN-0": [][]string{{"!", "-d", "10.10.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-1"}},
				"AWS-SNAT-CHAIN-1": [][]string{{"!", "-d", "10.11.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-2"}},
				"AWS-SNAT-CHAIN-2": [][]string{{"!", "-d", "10.12.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN EXCLUSION", "-j", "AWS-SNAT-CHAIN-3"}},
				"AWS-SNAT-CHAIN-3": [][]string{{"!", "-d", "10.13.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN EXCLUSION", "-j", "AWS-SNAT-CHAIN-4"}},
				"AWS-SNAT-CHAIN-4": [][]string{{"-m", "comment", "--comment", "AWS, SNAT", "-m", "addrtype", "!", "--dst-type", "LOCAL", "-j", "SNAT", "--to-source", "10.10.10.20"}},
				"POSTROUTING":      [][]string{{"-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-0"}}},
			"mangle": {
				"PREROUTING": [][]string{
					{"-m", "comment", "--comment", "AWS, primary ENI", "-i", "lo", "-m", "addrtype", "--dst-type", "LOCAL", "--limit-iface-in", "-j", "CONNMARK", "--set-mark", "0x80/0x80"},
					{"-m", "comment", "--comment", "AWS, primary ENI", "-i", "eni+", "-j", "CONNMARK", "--restore-mark", "--mask", "0x80"},
				},
			},
		}, mockIptables.dataplaneState)
}

func TestSetupHostNetworkCleansUpStaleSNATRules(t *testing.T) {
	ctrl, mockNetLink, _, mockNS, mockIptables := setup(t)
	defer ctrl.Finish()

	var mockRPFilter mockFile
	ln := &linuxNetwork{
		useExternalSNAT:        false,
		excludeSNATCIDRs:       nil,
		nodePortSupportEnabled: true,
		mainENIMark:            defaultConnmark,

		netLink: mockNetLink,
		ns:      mockNS,
		newIptables: func() (iptablesIface, error) {
			return mockIptables, nil
		},
		openFile: func(name string, flag int, perm os.FileMode) (stringWriteCloser, error) {
			return &mockRPFilter, nil
		},
	}

	var hostRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&hostRule)
	mockNetLink.EXPECT().RuleDel(&hostRule)
	var mainENIRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&mainENIRule)
	mockNetLink.EXPECT().RuleDel(&mainENIRule)
	mockNetLink.EXPECT().RuleAdd(&mainENIRule)

	vpcCIDRs := []*string{aws.String("10.10.0.0/16"), aws.String("10.11.0.0/16")}
	_ = mockIptables.Append("nat", "AWS-SNAT-CHAIN-0", "!", "-d", "10.10.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAN", "-j", "AWS-SNAT-CHAIN-1") //AWS SNAT CHAN proves backwards compatibility
	_ = mockIptables.Append("nat", "AWS-SNAT-CHAIN-1", "!", "-d", "10.11.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-2")
	_ = mockIptables.Append("nat", "AWS-SNAT-CHAIN-2", "!", "-d", "10.12.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN EXCLUSION", "-j", "AWS-SNAT-CHAIN-3")
	_ = mockIptables.Append("nat", "AWS-SNAT-CHAIN-3", "!", "-d", "10.13.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN EXCLUSION", "-j", "AWS-SNAT-CHAIN-4")
	_ = mockIptables.Append("nat", "AWS-SNAT-CHAIN-4", "-m", "comment", "--comment", "AWS, SNAT", "-m", "addrtype", "!", "--dst-type", "LOCAL", "-j", "SNAT", "--to-source", "10.10.10.20")
	_ = mockIptables.NewChain("nat", "AWS-SNAT-CHAIN-5")
	_ = mockIptables.Append("nat", "POSTROUTING", "-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-0")

	err := ln.SetupHostNetwork(testENINetIPNet, vpcCIDRs, "", &testENINetIP)
	assert.NoError(t, err)

	assert.Equal(t,
		map[string]map[string][][]string{
			"nat": {
				"AWS-SNAT-CHAIN-0": [][]string{{"!", "-d", "10.10.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-1"}},
				"AWS-SNAT-CHAIN-1": [][]string{{"!", "-d", "10.11.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-2"}},
				"AWS-SNAT-CHAIN-2": [][]string{{"-m", "comment", "--comment", "AWS, SNAT", "-m", "addrtype", "!", "--dst-type", "LOCAL", "-j", "SNAT", "--to-source", "10.10.10.20"}},
				"AWS-SNAT-CHAIN-3": [][]string{},
				"AWS-SNAT-CHAIN-4": [][]string{},
				"POSTROUTING":      [][]string{{"-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-0"}}},
			"mangle": {
				"PREROUTING": [][]string{
					{"-m", "comment", "--comment", "AWS, primary ENI", "-i", "lo", "-m", "addrtype", "--dst-type", "LOCAL", "--limit-iface-in", "-j", "CONNMARK", "--set-mark", "0x80/0x80"},
					{"-m", "comment", "--comment", "AWS, primary ENI", "-i", "eni+", "-j", "CONNMARK", "--restore-mark", "--mask", "0x80"},
				},
			},
		}, mockIptables.dataplaneState)
}

func TestSetupHostNetworkExcludedSNATCIDRsIdempotent(t *testing.T) {
	ctrl, mockNetLink, _, mockNS, mockIptables := setup(t)
	defer ctrl.Finish()

	var mockRPFilter mockFile
	ln := &linuxNetwork{
		useExternalSNAT:        false,
		excludeSNATCIDRs:       []string{"10.12.0.0/16", "10.13.0.0/16"},
		nodePortSupportEnabled: true,
		mainENIMark:            defaultConnmark,

		netLink: mockNetLink,
		ns:      mockNS,
		newIptables: func() (iptablesIface, error) {
			return mockIptables, nil
		},
		openFile: func(name string, flag int, perm os.FileMode) (stringWriteCloser, error) {
			return &mockRPFilter, nil
		},
	}

	var hostRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&hostRule)
	mockNetLink.EXPECT().RuleDel(&hostRule)
	var mainENIRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&mainENIRule)
	mockNetLink.EXPECT().RuleDel(&mainENIRule)
	mockNetLink.EXPECT().RuleAdd(&mainENIRule)

	_ = mockIptables.Append("nat", "AWS-SNAT-CHAIN-0", "!", "-d", "10.10.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-1")
	_ = mockIptables.Append("nat", "AWS-SNAT-CHAIN-1", "!", "-d", "10.11.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-2")
	_ = mockIptables.Append("nat", "AWS-SNAT-CHAIN-2", "!", "-d", "10.12.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN EXCLUSION", "-j", "AWS-SNAT-CHAIN-3")
	_ = mockIptables.Append("nat", "AWS-SNAT-CHAIN-3", "!", "-d", "10.13.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN EXCLUSION", "-j", "AWS-SNAT-CHAIN-4")
	_ = mockIptables.Append("nat", "AWS-SNAT-CHAIN-4", "-m", "comment", "--comment", "AWS, SNAT", "-m", "addrtype", "!", "--dst-type", "LOCAL", "-j", "SNAT", "--to-source", "10.10.10.20")
	_ = mockIptables.Append("nat", "POSTROUTING", "-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-0")

	// remove exclusions
	vpcCIDRs := []*string{aws.String("10.10.0.0/16"), aws.String("10.11.0.0/16")}
	err := ln.SetupHostNetwork(testENINetIPNet, vpcCIDRs, "", &testENINetIP)
	assert.NoError(t, err)

	assert.Equal(t,
		map[string]map[string][][]string{
			"nat": {
				"AWS-SNAT-CHAIN-0": [][]string{{"!", "-d", "10.10.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-1"}},
				"AWS-SNAT-CHAIN-1": [][]string{{"!", "-d", "10.11.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-2"}},
				"AWS-SNAT-CHAIN-2": [][]string{{"!", "-d", "10.12.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN EXCLUSION", "-j", "AWS-SNAT-CHAIN-3"}},
				"AWS-SNAT-CHAIN-3": [][]string{{"!", "-d", "10.13.0.0/16", "-m", "comment", "--comment", "AWS SNAT CHAIN EXCLUSION", "-j", "AWS-SNAT-CHAIN-4"}},
				"AWS-SNAT-CHAIN-4": [][]string{{"-m", "comment", "--comment", "AWS, SNAT", "-m", "addrtype", "!", "--dst-type", "LOCAL", "-j", "SNAT", "--to-source", "10.10.10.20"}},
				"POSTROUTING":      [][]string{{"-m", "comment", "--comment", "AWS SNAT CHAIN", "-j", "AWS-SNAT-CHAIN-0"}}},
			"mangle": {
				"PREROUTING": [][]string{
					{"-m", "comment", "--comment", "AWS, primary ENI", "-i", "lo", "-m", "addrtype", "--dst-type", "LOCAL", "--limit-iface-in", "-j", "CONNMARK", "--set-mark", "0x80/0x80"},
					{"-m", "comment", "--comment", "AWS, primary ENI", "-i", "eni+", "-j", "CONNMARK", "--restore-mark", "--mask", "0x80"},
				},
			},
		}, mockIptables.dataplaneState)
}

func TestSetupHostNetworkMultipleCIDRs(t *testing.T) {
	ctrl, mockNetLink, _, mockNS, mockIptables := setup(t)
	defer ctrl.Finish()

	var mockRPFilter mockFile
	ln := &linuxNetwork{
		useExternalSNAT:        true,
		nodePortSupportEnabled: true,
		mainENIMark:            defaultConnmark,

		netLink: mockNetLink,
		ns:      mockNS,
		newIptables: func() (iptablesIface, error) {
			return mockIptables, nil
		},
		openFile: func(name string, flag int, perm os.FileMode) (stringWriteCloser, error) {
			return &mockRPFilter, nil
		},
	}

	var hostRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&hostRule)
	mockNetLink.EXPECT().RuleDel(&hostRule)
	var mainENIRule netlink.Rule
	mockNetLink.EXPECT().NewRule().Return(&mainENIRule)
	mockNetLink.EXPECT().RuleDel(&mainENIRule)
	mockNetLink.EXPECT().RuleAdd(&mainENIRule)

	var vpcCIDRs []*string
	vpcCIDRs = []*string{aws.String("10.10.0.0/16"), aws.String("10.11.0.0/16")}
	err := ln.SetupHostNetwork(testENINetIPNet, vpcCIDRs, "", &testENINetIP)
	assert.NoError(t, err)
}

func TestIncrementIPv4Addr(t *testing.T) {
	testCases := []struct {
		name     string
		ip       net.IP
		expected net.IP
		err      bool
	}{
		{"increment", net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2).To4(), false},
		{"carry up 1", net.IPv4(10, 0, 0, 255), net.IPv4(10, 0, 1, 0).To4(), false},
		{"carry up 2", net.IPv4(10, 0, 255, 255), net.IPv4(10, 1, 0, 0).To4(), false},
		{"overflow", net.IPv4(255, 255, 255, 255), nil, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := incrementIPv4Addr(tc.ip)
			if tc.err {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.expected, result, tc.name)
		})
	}
}

type mockIptables struct {
	// dataplaneState is a map from table name to chain name to slice of rulespecs
	dataplaneState map[string]map[string][][]string
}

func newMockIptables() *mockIptables {
	return &mockIptables{dataplaneState: map[string]map[string][][]string{}}
}

func (ipt *mockIptables) Exists(table, chainName string, rulespec ...string) (bool, error) {
	chain := ipt.dataplaneState[table][chainName]
	for _, r := range chain {
		if reflect.DeepEqual(rulespec, r) {
			return true, nil
		}
	}
	return false, nil
}

func (ipt *mockIptables) Insert(table, chain string, pos int, rulespec ...string) error {
	return nil
}

func (ipt *mockIptables) Append(table, chain string, rulespec ...string) error {
	if ipt.dataplaneState[table] == nil {
		ipt.dataplaneState[table] = map[string][][]string{}
	}
	ipt.dataplaneState[table][chain] = append(ipt.dataplaneState[table][chain], rulespec)
	return nil
}

func (ipt *mockIptables) Delete(table, chainName string, rulespec ...string) error {
	chain := ipt.dataplaneState[table][chainName]
	updatedChain := chain[:0]
	found := false
	for _, r := range chain {
		if !found && reflect.DeepEqual(rulespec, r) {
			found = true
			continue
		}
		updatedChain = append(updatedChain, r)
	}
	if !found {
		return errors.New("not found")
	}
	ipt.dataplaneState[table][chainName] = updatedChain
	return nil
}

func (ipt *mockIptables) List(table, chain string) ([]string, error) {
	var chains []string
	chainContents := ipt.dataplaneState[table][chain]
	for _, ruleSpec := range chainContents {
		sanitizedRuleSpec := []string{"-A", chain}
		for _, item := range ruleSpec {
			if strings.Contains(item, " ") {
				item = fmt.Sprintf("%q", item)
			}
			sanitizedRuleSpec = append(sanitizedRuleSpec, item)
		}
		chains = append(chains, strings.Join(sanitizedRuleSpec, " "))
	}
	return chains, nil

}

func (ipt *mockIptables) NewChain(table, chain string) error {
	return nil
}

func (ipt *mockIptables) ClearChain(table, chain string) error {
	return nil
}

func (ipt *mockIptables) DeleteChain(table, chain string) error {
	return nil
}

func (ipt *mockIptables) ListChains(table string) ([]string, error) {
	var chains []string
	for chain := range ipt.dataplaneState[table] {
		chains = append(chains, chain)
	}
	return chains, nil
}

func (ipt *mockIptables) HasRandomFully() bool {
	// TODO: Work out how to write a test case for this
	return true
}

type mockFile struct {
	closed bool
	data   string
}

func (f *mockFile) WriteString(s string) (int, error) {
	if f.closed {
		panic("write call on closed file")
	}
	f.data += s
	return len(s), nil
}

func (f *mockFile) Close() error {
	if f.closed {
		panic("close call on closed file")
	}
	f.closed = true
	return nil
}
