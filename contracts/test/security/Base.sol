// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {Test} from "forge-std/Test.sol";
import {XmrSwapV3} from "../../src/XmrSwapV3.sol";
import {SeshRegistry} from "../../src/registry/SeshRegistry.sol";
import {ExitLease} from "../../src/exits/ExitLease.sol";
import {MockUSDC} from "../../src/MockUSDC.sol";
import {MockAggregatorV3} from "../../src/MockAggregatorV3.sol";

/// Shared deployment for the security suite. Testnet placeholder amounts,
/// same as script/DeployRegistry.s.sol.
abstract contract SecBase is Test {
    uint256 constant BOB_S = 0x088fba2ab5511efb59a49f0f8e1ceceeb302ad1f60a4007702607c789719eaf5;
    uint256 constant BOB_K = 0xd51fb7a1b6932d11ee69a0e977c3c3bc0f3be706735daf55f6c11050b5c0c96b;
    uint256 constant ALICE_S = 0x0a4c454746ce6afc23d46643a0f26aba063b6af57206da911d09552be6b47a25;
    uint256 constant ALICE_K = 0x01c2a86a450bfac25beed06f52bc6b0ce204bcff0671285c93269f341c8e0254;

    uint256 constant SITE_BOND = 0.0002 ether;
    uint256 constant MAKER_BOND = 0.002 ether;
    uint256 constant CAP = 5;
    uint16 constant PEN_BPS = 500;
    uint256 constant MIN_PEN = 0.00005 ether;
    uint256 constant CH_BOND = 0.0001 ether;
    uint64 constant EXIT_DELAY = 3 days;
    uint64 constant CH_WINDOW = 2 days;
    uint64 constant ATTEST = 10 minutes;

    function deployAll() internal returns (XmrSwapV3 v3, SeshRegistry reg, ExitLease el, MockUSDC usdc) {
        v3 = new XmrSwapV3();
        usdc = new MockUSDC();
        MockAggregatorV3 feed = new MockAggregatorV3("ETH / USD", 3000e8);
        reg = new SeshRegistry(
            SeshRegistry.Params({
                swap: address(v3),
                siteMinBond: SITE_BOND,
                makerMinBond: MAKER_BOND,
                capFactor: CAP,
                penaltyBps: PEN_BPS,
                minPenalty: MIN_PEN,
                challengeBond: CH_BOND,
                exitDelay: EXIT_DELAY,
                challengeWindow: CH_WINDOW,
                minAttest: ATTEST,
                maxAttest: 30 minutes,
                usdc: address(usdc),
                usdcDecimals: 6,
                ethUsdFeed: address(feed),
                maxFeedAge: 2 days
            })
        );
        el = new ExitLease(reg, address(usdc));
    }
}
