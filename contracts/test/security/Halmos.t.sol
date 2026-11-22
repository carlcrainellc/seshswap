// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {SecBase} from "./Base.sol";
import {XmrSwapV3} from "../../src/XmrSwapV3.sol";
import {SeshRegistry} from "../../src/registry/SeshRegistry.sol";
import {ExitLease} from "../../src/exits/ExitLease.sol";
import {MockUSDC} from "../../src/MockUSDC.sol";

/// Symbolic checks (halmos): run with
///   halmos --match-contract HalmosChecks
/// Each check_ holds for ALL inputs, not just sampled ones.
contract HalmosChecks is SecBase {
    XmrSwapV3 v3;
    SeshRegistry reg;
    ExitLease el;
    MockUSDC usdc;
    address seller = address(0x5E11);
    address chal = address(0xC4A1);
    uint256 exitId;

    function setUp() public {
        vm.warp(1_000_000);
        (v3, reg, el, usdc) = deployAll();
        vm.deal(seller, 1 ether);
        vm.deal(chal, 1 ether);
        vm.prank(seller);
        exitId = reg.register{value: SITE_BOND}("exit1.sesh", "exit", "");
    }

    function check_leaseQuoteExactNoOverflow(uint128 wph, uint128 uph, uint32 h) public {
        vm.assume(wph != 0 && uph != 0 && h >= 1 && h <= 744);
        vm.prank(seller);
        el.setOffer(exitId, address(0x9A70), wph, uph, 744);
        assert(el.quote(exitId, address(0), h) == uint256(wph) * h);
        assert(el.quote(exitId, address(usdc), h) == uint256(uph) * h);
    }

    function check_setOfferOnlyOwner(address who, address p, uint128 wph, uint32 mh) public {
        vm.assume(who != seller);
        vm.prank(who);
        try el.setOffer(exitId, p, wph, 0, mh) { assert(false); } catch {}
    }

    function check_answerOnlyBeforeDeadline(uint64 dt) public {
        vm.prank(chal);
        reg.challenge{value: CH_BOND}(exitId);
        uint64 dl = reg.listing(exitId).challengeDeadline;
        vm.warp(block.timestamp + dt);
        vm.prank(seller);
        try reg.answer(exitId) { assert(block.timestamp < dl); } catch {}
    }

    function check_resolveOnlyFromDeadline(uint64 dt) public {
        vm.prank(chal);
        reg.challenge{value: CH_BOND}(exitId);
        uint64 dl = reg.listing(exitId).challengeDeadline;
        vm.warp(block.timestamp + dt);
        try reg.resolveChallenge(exitId) { assert(block.timestamp >= dl); } catch {}
    }

    function check_withdrawOnlyAfterDelay(uint64 dt, address to) public {
        vm.assume(to != address(0));
        vm.prank(seller);
        reg.requestExit(exitId);
        uint64 at = reg.listing(exitId).exitAt;
        vm.warp(block.timestamp + dt);
        vm.prank(seller);
        try reg.withdraw(exitId, to) { assert(block.timestamp >= at); } catch {}
    }

    function check_setReadyOnlyBeforeT1ByOwner(uint64 dt, address who) public {
        address alice = address(0xA11CE);
        vm.deal(alice, 1 ether);
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(alice, address(0xB0B), BOB_K, ALICE_K,
            uint64(block.timestamp) + 1 hours, uint64(block.timestamp) + 2 hours, 1 hours, address(0), 1 ether, 0, 0);
        vm.prank(alice);
        v3.lock{value: 1 ether}(address(0xB0B), BOB_K, ALICE_K, 1 hours, 1 hours, 1 hours, address(0), 1 ether, 0, 0);
        vm.warp(block.timestamp + dt);
        vm.prank(who);
        try v3.setReady(s) { assert(who == alice && block.timestamp < s.t1); } catch {}
    }

    /// M-1: cancelIntent succeeds only for the owner, from lock time + LOCK_IN until t1.
    function check_cancelOnlyAfterLockIn(uint64 dt, address who) public {
        address alice = address(0xA11CE);
        vm.deal(alice, 1 ether);
        uint256 t0 = block.timestamp;
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(alice, address(0xB0B), BOB_K, ALICE_K,
            uint64(t0) + 1 hours, uint64(t0) + 2 hours, 1 hours, address(0), 1 ether, 0, 0);
        vm.prank(alice);
        v3.lock{value: 1 ether}(address(0xB0B), BOB_K, ALICE_K, 1 hours, 1 hours, 1 hours, address(0), 1 ether, 0, 0);
        vm.warp(t0 + dt);
        vm.prank(who);
        try v3.cancelIntent(s) {
            assert(who == alice && block.timestamp >= t0 + v3.LOCK_IN() && block.timestamp < s.t1);
        } catch {}
    }
}
